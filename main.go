package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocolly/colly"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	_ "modernc.org/sqlite"
)

//go:generate stringer -type=cinema
type cinema int

const (
	Agrafka cinema = iota
	CCityBonarka
	CCityKazimierz
	CCityZakopianka
	Kijow
	Kika
	Mikro
	PodBaranami
	Multikino
	Paradox
	Sfinks
	LAST
)

type showing struct {
	cinema cinema
	time   time.Time
}

type siteConfig struct {
	rootSel    string
	linkSel    string
	charSet    string
	processFun func(cinema, chan result)
}

type site struct {
	cinema cinema
	config siteConfig
}

type result struct {
	cinema cinema
	movies map[string][]showing
	// titleToTime map[string][]time.Time
}

var repertoires = map[cinema]string{
	Agrafka:     "https://bilety.kinoagrafka.pl",
	Kijow:       "https://kupbilet.kijow.pl/MSI/mvc/pl?sort=Date&date=1970-01&datestart=0",
	Kika:        "https://bilety.kinokika.pl",
	Mikro:       "https://kinomikro.pl/repertoire/?view=all",
	Paradox:     "https://kinoparadox.pl/repertuar/",
	PodBaranami: "https://kinopodbaranami.pl/repertuar.php",
	Sfinks:      "https://kinosfinks.okn.edu.pl/wydarzenia-szukaj-strona-1.html",
}

var cinemaApiIds = map[cinema]string{
	Multikino:       "0005",
	CCityBonarka:    "1090",
	CCityKazimierz:  "1076",
	CCityZakopianka: "1064",
}

var apiUrls = map[string]string{
	"MultikinoCookies":    "https://multikino.pl/api/microservice",
	"MultikinoFilmsStart": "https://multikino.pl/api/microservice/showings/cinemas/",
	"MultikinoFilmsEnd":   "/films/",
	"CCityDatesStart":     "https://cinema-city.pl/pl/data-api-service/v1/quickbook/10103/dates/in-cinema/",
	"CCityDatesEnd":       "/until/",
	"CCityFilmsStart":     "https://cinema-city.pl/pl/data-api-service/v1/quickbook/10103/film-events/in-cinema/",
	"CCityFilmsEnd":       "/at-date/",
}

var excludedByKeywords = [...]string{"UKRAINIAN", "UKRAIŃSKI", "DLA OSÓB", "KLUB SENIORA", "DKF KROPKA DLA DZIECI"}

// TODO properly remove punctuation?
var removedKeywords = [...]string{"2D", "3D", "- DUBBING", "DUBBING", " - NAPISY", "NAPISY",
	"TANI WTOREK:", "DKF KROPKA:", "DKF PEŁNA SALA:",
	"- PRZEDPREMIERA", "+ ENG SUB", "- POKAZ SPECJALNY Z DYSKUSJĄ", "– WERSJA REŻYSERSKA",
	". WERSJA REŻYSERSKA", "- POKAZ SPECJALNY", "POKAZ SPECJALNY"}

var timeRegex = regexp.MustCompile("^(([0-1]?[0-9])|(2[0-3]))(:[0-5][0-9])+$")

func main() {
	originFlagPtr := flag.String("origin", "", "The gotify origin \"scheme://authority\".")
	tokenFlagPtr := flag.String("token", "", "The Gotify token.")
	logFlagPtr := flag.Bool("log", false, "Determines if the result should be logged.")
	flag.Parse()

	db, err := sql.Open("sqlite", "./movies.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	sqlCreate := `
	CREATE TABLE IF NOT EXISTS movies (
		title TEXT NOT NULL PRIMARY KEY,
		original_title TEXT,
		first_showing TEXT NOT NULL,
		latest_showing TEXT NOT NULL
	);
	`
	_, err = db.Exec(sqlCreate)
	if err != nil {
		panic(err)
	}

	cinemasToScrape := [...]site{
		{Agrafka,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Kijow,
			siteConfig{
				rootSel: "div.cd-timeline-block",
				linkSel: "a[href].eventcard.col-6"}},
		{Kika,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Paradox,
			siteConfig{
				rootSel: "div.list-item__content__row"}},
		{Mikro,
			siteConfig{
				rootSel: "section.row"}},
		{PodBaranami,
			siteConfig{
				rootSel: "li[title]",
				charSet: "iso-8859-2"}},
		{Sfinks,
			siteConfig{
				rootSel: "span.zajawka",
				linkSel: "a[href][title^='Strona']"}},
	}

	cinemasToFetch := [...]site{
		{Multikino,
			siteConfig{
				processFun: fetchMultikino}},
		{CCityBonarka,
			siteConfig{
				processFun: fetchCCity}},
		{CCityKazimierz,
			siteConfig{
				processFun: fetchCCity}},
		{CCityZakopianka,
			siteConfig{
				processFun: fetchCCity}},
	}

	answerCountdown := len(cinemasToScrape) + len(cinemasToFetch)
	resultCh := make(chan result)

	for _, cinema := range cinemasToScrape {
		go scrapeCinema(cinema, resultCh)
	}
	for _, cinema := range cinemasToFetch {
		go cinema.config.processFun(cinema.cinema, resultCh)
	}

	var receivedArr = [LAST]bool{}
	movies := map[string][]showing{}
	titles := []string{}

WaitForCinemas:
	for {
		var result result
		select {
		case result = <-resultCh:

		case <-time.After(10 * time.Second):
			break WaitForCinemas
		}

		receivedArr[result.cinema] = true
		var lenT int

		for k, v := range result.movies {
			title := strings.ToUpper(k)

			for _, kw := range excludedByKeywords {
				if strings.Contains(title, kw) {
					goto skipMovie
				}
			}

			for _, kw := range removedKeywords {
				title = strings.ReplaceAll(title, kw, "")
			}
			title = strings.TrimSpace(title)

			// remove any text in parentheses at the end like '(dubbing)'
			lenT = len(title)
			for title[lenT-1] == ')' && title[0] != '(' {
				for i := range lenT {
					if title[lenT-1-i] == '(' {
						title = title[0 : lenT-1-i-1]
						break
					}
				}
				lenT = len(title)
			}
			title = strings.TrimSpace(title)

			if showings, ok := movies[title]; ok {
				movies[title] = append(showings, v...)
			} else {
				movies[title] = v
				titles = append(titles, title)
			}

		skipMovie:
		}

		answerCountdown -= 1
		if answerCountdown == 0 {
			break
		}
	}

	for _, movie := range movies {
		sort.Slice(movie, func(a, b int) bool {
			return movie[a].time.Before(movie[b].time)
		})
	}

	moviesToday := map[string][]showing{}
	moviesYesterday := map[string][]showing{}
	moviesLastWeek := map[string][]showing{}
	moviesRest := map[string][]showing{}
	for title, showings := range movies {
		sqlSelect := `
		SELECT latest_showing
		 FROM movies
		 WHERE title = ?;
		`
		rows, err := db.Query(sqlSelect, title)
		if err != nil {
			panic(err)
		}

		today := time.Now()
		todayStr :=
			fmt.Sprintf("%d-%02d-%02d",
				today.Year(), today.Month(), today.Day())
		if !rows.Next() {
			moviesToday[title] = showings

			sqlInsert := `
			INSERT INTO movies
			 (title, original_title, first_showing, latest_showing)
			 VALUES(?, ?, ?, ?)
			`
			_, err = db.Exec(sqlInsert, title, "", todayStr, todayStr)
			if err != nil {
				panic(err)
			}
		} else {
			var latestShowingStr string
			err := rows.Scan(&latestShowingStr)
			if err != nil {
				panic(err)
			}
			rows.Close()

			latestShowing, _ := time.Parse(time.DateOnly, latestShowingStr)
			today, _ := time.Parse(time.DateOnly, todayStr)
			hourDiff := today.Sub(latestShowing).Hours()

			if hourDiff <= 50 {
				moviesYesterday[title] = showings
			} else if hourDiff <= 170 {
				moviesLastWeek[title] = showings
			} else {
				moviesRest[title] = showings
			}

			sqlUpdate := `
			UPDATE movies
			 SET latest_showing = ?
			 WHERE title = ?
			`
			_, err = db.Exec(sqlUpdate, todayStr, title)
			if err != nil {
				panic(err)
			}
		}
	}

	var sb strings.Builder

	if len(moviesToday) > 0 {
		sb.WriteString("||TODAY||\n")
		writeMovies(&sb, moviesToday)
	}

	if len(moviesYesterday) > 0 {
		sb.WriteString("||YESTERDAY||\n")
		writeMovies(&sb, moviesYesterday)
	}

	if len(moviesLastWeek) > 0 {
		sb.WriteString("||LAST WEEK||\n")
		writeMovies(&sb, moviesLastWeek)
	}

	if len(moviesRest) > 0 {
		sb.WriteString("||ALL OTHERS||\n")
		writeMovies(&sb, moviesRest)
	}

	totalLine := fmt.Sprintf("TOTAL: %d \n", len(titles))
	sb.WriteString(totalLine)

	if slices.Contains(receivedArr[:], false) {
		sb.WriteString("RESULTS NOT RECEIVED FROM:\n")
		for cinemaIndex, received := range receivedArr {
			if !received {
				cinemaLine := fmt.Sprintf("%s\n", cinema(cinemaIndex))
				sb.WriteString(cinemaLine)
			}
		}
	}

	if *originFlagPtr != "" && *tokenFlagPtr != "" {
		uv := url.Values{}
		today := time.Now()
		todayStr :=
			fmt.Sprintf("%02d/%02d/%d\n",
				today.Day(), today.Month(), today.Year())
		uv.Set("title", todayStr)
		uv.Add("message", sb.String())

		gotifyUrl := fmt.Sprintf("%s/message?token=%s", *originFlagPtr, *tokenFlagPtr)
		http.PostForm(gotifyUrl, uv)
	}

	if *logFlagPtr {
		fmt.Println(sb.String())
	}
}

func fetchMultikino(cinema cinema, resultCh chan result) {
	client := &http.Client{}

	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Println(err)
	}
	client.Jar = jar

	// Obtain cookies
	req, _ := http.NewRequest("GET", apiUrls["MultikinoCookies"], nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}
	res.Body.Close()

	filmsUrl := apiUrls["MultikinoFilmsStart"] + cinemaApiIds[cinema] + apiUrls["MultikinoFilmsEnd"]
	req, _ = http.NewRequest("GET", filmsUrl, nil)
	res, err = client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}
	defer res.Body.Close()

	var body map[string]any
	bodyBytes, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		log.Println(err)
		return
	}

	movies := make(map[string][]showing)
	moviesJson := body["result"].([]any)

	for _, e := range moviesJson {
		movieJson := e.(map[string]any)
		title := movieJson["filmTitle"].(string)
		showings := []showing{}
		showingGroupsJson := movieJson["showingGroups"].([]any)

		for _, e := range showingGroupsJson {
			showingGroupJson := e.(map[string]any)
			sessionsJson := showingGroupJson["sessions"].([]any)

			for _, e := range sessionsJson {
				sessionJson := e.(map[string]any)
				timeString := sessionJson["startTime"].(string)
				time := processDateTimeString(timeString, cinema)
				showings = append(showings, showing{cinema: Multikino, time: time})
			}
		}
		movies[title] = showings
	}

	resultCh <- result{cinema: Multikino, movies: movies}
}

func fetchCCity(cinema cinema, resultCh chan result) {
	client := &http.Client{}

	datesBasePath := apiUrls["CCityDatesStart"] + cinemaApiIds[cinema] + apiUrls["CCityDatesEnd"]
	now := time.Now()
	today := fmt.Sprintf("%d-%02d-%02d", now.Year()+1, now.Month(), now.Day())
	datesTodayPath := datesBasePath + today

	req, _ := http.NewRequest("GET", datesTodayPath, nil)
	res, err := client.Do(req)
	if err != nil {
		// TODO panics maybe?
		log.Println(err)
		return
	}

	var body map[string]any
	bodyBytes, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		log.Println(err)
		return
	}

	body = body["body"].(map[string]any)
	dateRaws := body["dates"].([]any)
	var dates = []string{}
	for _, dateRaw := range dateRaws {
		dates = append(dates, dateRaw.(string))
	}
	res.Body.Close()

	moviesCh := make(chan map[string][]showing)
	for _, date := range dates {
		go fetchCCityDay(cinema, date, moviesCh)
	}

	movies := make(map[string][]showing)
	answerCountdown := len(dates)

	// unnecessary
WaitForCCityDay:
	for answerCountdown > 0 {
		var dayMovies map[string][]showing
		select {
		case dayMovies = <-moviesCh:

		case <-time.After(5 * time.Second):
			break WaitForCCityDay
		}

		for dayTitle, dayShowings := range dayMovies {
			if showings, ok := movies[dayTitle]; ok {
				movies[dayTitle] = append(showings, dayShowings...)
			} else {
				movies[dayTitle] = dayShowings
			}
		}
		answerCountdown -= 1
	}

	resultCh <- result{cinema: cinema, movies: movies}
}

func fetchCCityDay(cinema cinema, date string, moviesCh chan map[string][]showing) {
	client := &http.Client{}
	moviesBasePath := apiUrls["CCityFilmsStart"] + cinemaApiIds[cinema] + apiUrls["CCityFilmsEnd"]
	req, _ := http.NewRequest("GET", moviesBasePath+date, nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}

	var body map[string]any
	bodyBytes, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		log.Println(err)
		return
	}
	body = body["body"].(map[string]any)

	idToTitle := make(map[string]string)
	films := body["films"].([]any)
	for _, film := range films {
		filmMap := film.(map[string]any)
		id := filmMap["id"].(string)
		title := filmMap["name"].(string)
		idToTitle[id] = title
	}

	movies := make(map[string][]showing)
	events := body["events"].([]any)
	for _, event := range events {
		eventMap := event.(map[string]any)
		id := eventMap["filmId"].(string)
		title := idToTitle[id]

		dateTimeStr := eventMap["eventDateTime"].(string)
		dateTime := processDateTimeString(dateTimeStr, cinema)

		showin := showing{cinema, dateTime}
		if showings, ok := movies["title"]; ok {
			movies[title] = append(showings, showin)
		} else {
			movies[title] = []showing{showin}
		}
	}

	moviesCh <- movies
}

func scrapeCinema(site site, resultCh chan result) {
	cinema := site.cinema
	config := site.config

	c := colly.NewCollector(
		colly.MaxDepth(2),
		colly.Async(true),
	)

	c.OnRequest(func(r *colly.Request) {
		if config.charSet != "" {
			r.ResponseCharacterEncoding = config.charSet
		}
	})

	movies := make(map[string][]showing)

	var lastDate string
	c.OnHTML(config.rootSel, func(e *colly.HTMLElement) {
		title := getTitle(cinema, e)
		if title == "" {
			return
		}

		// TODO proper error handling
		dateTime := getDateTime(cinema, e, &lastDate)

		if !dateTime.Before(time.Now().Local()) {
			movies[title] = append(movies[title], showing{cinema, dateTime})
		}
	})

	if config.linkSel != "" {
		c.OnHTML(config.linkSel, func(e *colly.HTMLElement) {
			link := getNextUrl(cinema, e)
			c.Visit(link)
		})
	}

	c.Visit(repertoires[cinema])
	c.Wait()

	resultCh <- result{cinema: cinema, movies: movies}
}

func processDateTimeString(rawDateTime string, cinema cinema) time.Time {
	dateTimeWords := strings.FieldsFunc(
		rawDateTime,
		func(r rune) bool {
			return slices.Contains([]rune{' ', '\n', '\t', 'T', '-', '.', '/', '_', '\'', ','}, r)
		})

	var (
		day, year, hour, minute int
		month                   time.Month
	)

	for _, dateTime := range dateTimeWords {
		// don't care for month-day or any other abomination of the sort
		if dateTimeInt, err := strconv.Atoi(dateTime); err == nil {
			if day == 0 && dateTimeInt <= 31 && !(year != 0 && month == 0) {
				day = dateTimeInt
			} else if month == 0 && dateTimeInt <= 31 {
				month = time.Month(dateTimeInt)
			} else if year == 0 && dateTimeInt > 2000 {
				year = dateTimeInt
			}
		} else if day != 0 && month == 0 {
			month = mapMonth(dateTime)
		} else if timeRegex.MatchString(dateTime) {
			hourMinuteSecond := strings.Split(dateTime, ":")
			hour, _ = strconv.Atoi(hourMinuteSecond[0])
			minute, _ = strconv.Atoi(hourMinuteSecond[1])
		}
	}

	if year == 0 {
		year = time.Now().Year()
		// just assume next year, if it's an earlier month
		if month < time.Now().Month() {
			year++
		}
	}

	location := time.Now().Location()

	return time.Date(year, month, day, hour, minute, 0, 0, location)
}

func mapMonth(monthStr string) time.Month {
	var month time.Month

	switch strings.ToLower(monthStr[:3]) {
	case "sty":
		month = time.January
	case "lut":
		month = time.February
	case "mar":
		month = time.March
	case "kwi":
		month = time.April
	case "maj":
		month = time.May
	case "cze":
		month = time.June
	case "lip":
		month = time.July
	case "sie":
		month = time.August
	case "wrz":
		month = time.September
	case "paz", "paź":
		month = time.October
	case "lis":
		month = time.November
	case "gru":
		month = time.December
	}

	return month
}

func getTitle(cinema cinema, e *colly.HTMLElement) string {
	var title string

	switch cinema {
	case Kika, Agrafka:
		title = e.DOM.Find("a").First().Text()

	case Kijow:
		title = e.DOM.Find("h2").After("i").Text()

	case PodBaranami:
		title = e.DOM.Find("a").First().Text()
		// lots of newlines and garbage around it
		title = strings.TrimSpace(title)

	case Paradox:
		title = e.DOM.Find("a.item-title").Text()

	case Sfinks:
		title = e.DOM.Find("span.title").Text()

	case Mikro:
		title = e.DOM.Find("a.repertoire-item-title").Text()
	}

	return title
}

func getDateTime(cinema cinema, e *colly.HTMLElement, lastDate *string) time.Time {
	var dateTimeStr string

	switch cinema {
	case Kika, Agrafka:
		dateRaw := e.DOM.Find("div.date").Text()
		dateLines := strings.Split(dateRaw, "\n")
		dateLines = dateLines[len(dateLines)-2:]
		dateTimeStr = strings.Join(dateLines, "")

	case Kijow:
		dateTimeStr = e.DOM.Find("span.cd-date").Text()

	case PodBaranami:
		timeRaw := e.DOM.Find("span").Find("a").Text()
		onclickStr, found := e.DOM.Find("span").Find("a").Attr("onclick")
		// times without URLs can just be skipped over
		if !found {
			return time.Time{}
		}

		onClickWords := strings.Split(onclickStr, ",")
		dateRaw := onClickWords[len(onClickWords)-5]
		dateTimeStr = dateRaw + " " + timeRaw

	case Paradox:
		dateRaw, _ := e.DOM.Attr("data-date")
		timeRaw := e.DOM.Find("div.item-time").Text()
		dateTimeStr = dateRaw + " " + timeRaw

	case Sfinks:
		dateTimeElement := e.DOM.Find("span.kali_data_od")
		dateRaw := dateTimeElement.Find("span").First().Text()
		timeRaw := dateTimeElement.Find("span").Eq(2).Text()
		dateTimeStr = dateRaw + " " + timeRaw

	case Mikro:
		dateElementMaybe := e.DOM.Find("div.repertoire-separator")
		if dateElementMaybe.Length() != 0 {
			*lastDate = dateElementMaybe.Text()
		}
		dateRaw := *lastDate
		timeRaw := e.DOM.Find("p.repertoire-item-hour").Text()
		dateTimeStr = dateRaw + " " + timeRaw
	}

	return processDateTimeString(dateTimeStr, cinema)
}

func getNextUrl(cinema cinema, e *colly.HTMLElement) string {
	var link string

	switch cinema {
	case Kijow, Sfinks:
		link = e.Request.AbsoluteURL(e.Attr("href"))
	}

	return link
}

func writeMovies(sb *strings.Builder, movies map[string][]showing) {
	titles := make([]string, len(movies))
	i := 0
	for title := range movies {
		titles[i] = title
		i++
	}
	collator := collate.New(language.Polish)
	collator.SortStrings(titles)

	var lastDate time.Time
	for _, title := range titles {
		titleLine := fmt.Sprintf("|%s|\n", title)
		sb.WriteString(titleLine)

		lastDate = time.Time{}
		for _, showing := range movies[title] {
			dateTime := showing.time
			date :=
				time.Date(dateTime.Year(), dateTime.Month(), dateTime.Day(),
					0, 0, 0, 0,
					dateTime.Location())

			if !lastDate.Equal(date) {
				dateLine :=
					fmt.Sprintf("======%02d/%02d/%d======\n",
						dateTime.Day(), dateTime.Month(), dateTime.Year())
				sb.WriteString(dateLine)
				lastDate = date
			}

			showingLine :=
				fmt.Sprintf("%s  %02d:%02d\n",
					showing.cinema.String(), dateTime.Hour(), dateTime.Minute())
			sb.WriteString(showingLine)
		}

		sb.WriteString("\n")
	}
}
