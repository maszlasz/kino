package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

//go:generate stringer -type=cinema
type cinema int

const (
	Agrafka cinema = iota
	CityBonarka
	CityKazimierz
	CityZakopianka
	IMAX
	Kijow
	Kika
	Mikro
	PodBaranami
	Multikino
	Paradox
	Sfinks
	LAST
)

var repertoires = map[cinema]string{
	Agrafka:     "https://bilety.kinoagrafka.pl",
	Kijow:       "https://kupbilet.kijow.pl/MSI/mvc/?sort=Date",
	Kika:        "https://bilety.kinokika.pl",
	Mikro:       "https://kinomikro.pl/repertoire/?view=all",
	Paradox:     "https://kinoparadox.pl/repertuar/",
	PodBaranami: "https://www.kinopodbaranami.pl/repertuar.php",
	Sfinks:      "https://kinosfinks.okn.edu.pl/wydarzenia.html"}

var cinemaApiIds = map[cinema]string{
	Multikino:      "0005",
	CityBonarka:    "1090",
	CityKazimierz:  "1076",
	CityZakopianka: "1064",
}

var apiUrls = map[string]string{
	"MultikinoJWT":        "https://www.multikino.pl/api/microservice",
	"MultikinoFilmsStart": "https://www.multikino.pl/api/microservice/showings/cinemas/",
	"MultikinoFilmsEnd":   "/films/",
	"CityDatesStart":      "https://www.cinema-city.pl/pl/data-api-service/v1/quickbook/10103/dates/in-cinema/",
	"CityDatesEnd":        "/until/",
	"CityFilmsStart":      "https://www.cinema-city.pl/pl/data-api-service/v1/quickbook/10103/film-events/in-cinema/",
	"CityFilmsEnd":        "/at-date/",
}

type showing struct {
	cinema cinema
	time   time.Time
}

type siteConfig struct {
	rootSel      string
	linkSel      string
	groupDateSel string
	groupItemSel string
	groupSel     string
	charSet      string
	processFun   func(cinema, chan result)
}

type site struct {
	cinema cinema
	config siteConfig
}

type result struct {
	cinema cinema
	movies map[string][]showing
}

// var moviesMx sync.Mutex
// var wg sync.WaitGroup
var excludedByKeywords = [...]string{"UKRAINIAN", "UKRAIŃSKI", "DKF KROPKA DLA", "KLUB SENIORA"}
var removedKeywords = [...]string{"2D", "3D", "DUBBING", "NAPISY", "TANI WTOREK:"}
var timeRegex = regexp.MustCompile("^(([0-1]?[0-9])|(2[0-3]))(:[0-5][0-9])+$")

func main() {
	cinemasToScrape := [...]site{
		{Kika,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Agrafka,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Kijow,
			siteConfig{
				rootSel: "div.cd-timeline-block",
				linkSel: "a[href].eventcard.col-6"}},
		{PodBaranami,
			siteConfig{
				rootSel: "li[title]",
				charSet: "iso-8859-2"}},
		{Paradox,
			siteConfig{
				rootSel: "div.list-item__content__row"}},
		{Sfinks,
			siteConfig{
				rootSel: "span.zajawka",
				linkSel: "a[title^='Strona'][href]"}},
		// {Paradox,
		// 	siteConfig{
		// 		groupDateSel:  "div.list-item__date",
		// 		groupTitleSel: "a.item-title",
		// 		groupSel:      "div.list-item"}},
		// {PodBaranami,
		// 	siteConfig{
		// 		// rootSel:      "li[title]",
		// 		groupDateSel: "p.rep_date",
		// 		groupItemSel: "li[title]",
		// 		groupSel:     "ul.program_list",
		// 		charSet:      "iso-8859-2"}},
	}

	cinemasToFetch := [...]site{
		{Multikino,
			siteConfig{
				processFun: parseMultikino}},
		{CityBonarka,
			siteConfig{
				processFun: parseCity}},
		{CityKazimierz,
			siteConfig{
				processFun: parseCity}},
		{CityZakopianka,
			siteConfig{
				processFun: parseCity}},
	}

	answerCountdown := len(cinemasToScrape) + len(cinemasToFetch)
	resultCh := make(chan result)

	for _, cinema := range cinemasToScrape {
		// timeouts
		go scrapeCinema(cinema, resultCh)
	}
	for _, cinema := range cinemasToFetch {
		// timeouts
		go cinema.config.processFun(cinema.cinema, resultCh)
	}

	var receivedArr = [LAST]bool{}
	movies := map[string][]showing{}
	titles := []string{}

	for {
		result := <-resultCh
		receivedArr[result.cinema] = true
		var lenT int

		for k, v := range result.movies {
			title := strings.ToUpper(k)

			for _, kw := range excludedByKeywords {
				if strings.Contains(title, kw) {
					goto skip
				}
			}

			for _, kw := range removedKeywords {
				title = strings.ReplaceAll(title, kw, "")
			}
			title = strings.TrimSpace(title)

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

		skip:
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

	collator := collate.New(language.Polish)
	collator.SortStrings(titles)
	// slices.Sort(titles)

	for _, title := range titles {
		fmt.Printf("|%s|\n", title)
		for _, showing := range movies[title] {
			fmt.Printf("%s: %s, ", showing.cinema.String(), showing.time)
		}
		fmt.Print("\n\n")
	}

	fmt.Printf("TOTAL: %d \n", len(titles))
}

func parseMultikino(cinema cinema, resultCh chan result) {
	client := &http.Client{}

	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Println(err)
	}
	client.Jar = jar

	// Obtain JWTs
	req, _ := http.NewRequest("GET", apiUrls["MultikinoJWT"], nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
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
		panic(err)
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

func parseCity(cinema cinema, resultCh chan result) {
	client := &http.Client{}

	// jar, err := cookiejar.New(nil)
	// if err != nil {
	// 	log.Println(err)
	// }
	// client.Jar = jar

	// Obtain JWTs
	// req, _ := http.NewRequest("GET", "https://www.cinema-city.pl", nil)
	// res, err := client.Do(req)
	// if err != nil {
	// 	log.Println(err)
	// }
	// res.Body.Close()

	// req, _ := http.NewRequest("GET", "https://www.cinema-city.pl/pl/data-api-service/v1/quickbook/10103/dates/in-cinema/1090/until/2026-11-30", nil)

	// res, err := client.Do(req)
	// if err != nil {
	// 	log.Println(err)
	// }
	// fmt.Println(res.Header)
	// fmt.Println(string(res.Body))
	// res.Body.Close()
	datesBasePath := apiUrls["CityDatesStart"] + cinemaApiIds[cinema] + apiUrls["CityDatesEnd"]
	now := time.Now()
	today := fmt.Sprintf("%d-%02d-%02d", now.Year()+1, now.Month(), now.Day())
	datesTodayPath := datesBasePath + today
	// fmt.Println(datesTodayPath)
	req, _ := http.NewRequest("GET", datesTodayPath, nil)
	// req, _ = http.NewRequest("GET", "https://www.multikino.pl/api/microservice/showings/cinemas/0005/films", nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}
	// fmt.Println(res.Header)

	var body map[string]any
	bodyBytes, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		panic(err)
	}
	// fmt.Println(body)
	body = body["body"].(map[string]any)
	dateRaws := body["dates"].([]any)
	var dates = []string{}
	for _, dateRaw := range dateRaws {
		dates = append(dates, dateRaw.(string))
	}
	res.Body.Close()
	// fmt.Println(dates)

	moviesCh := make(chan map[string][]showing)
	for _, date := range dates {
		go parseCityDay(cinema, date, moviesCh)
	}

	movies := make(map[string][]showing)
	answerCountdown := len(dates)
	for answerCountdown > 0 {
		dayMovies := <-moviesCh

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

func parseCityDay(cinema cinema, date string, moviesCh chan map[string][]showing) {
	client := &http.Client{}
	moviesBasePath := apiUrls["CityFilmsStart"] + cinemaApiIds[cinema] + apiUrls["CityFilmsEnd"]
	req, _ := http.NewRequest("GET", moviesBasePath+date, nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}

	var body map[string]any
	bodyBytes, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		panic(err)
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
		colly.DetectCharset(),
	)
	// c.RedirectHandler = redirect

	c.OnRequest(func(r *colly.Request) {
		if config.charSet != "" {
			r.ResponseCharacterEncoding = config.charSet
		}
		fmt.Println("Site: ", r.URL.String())
	})

	movies := make(map[string][]showing)
	var moviesMx sync.Mutex

	if cinema == Kijow {
		c.OnResponse(func(r *colly.Response) {
			file, fileErr := os.Create("file.txt")
			if fileErr != nil {
				fmt.Println(fileErr)
				return
			}
			fmt.Fprintf(file, "%v\n", r.Headers)
			fmt.Fprintf(file, "%v\n", string(r.Body))
		})
	}

	if config.groupDateSel != "" && config.groupSel != "" {
		groupDates := []string{}
		groupDateCounter := 0
		c.OnHTML(config.groupDateSel, func(e *colly.HTMLElement) {
			// call getDateTimes
			groupDates = append(groupDates, e.DOM.Text())
		})

		c.OnHTML(config.groupSel, func(e *colly.HTMLElement) {
			// fmt.Println(groupDates[groupDateCounter])
			e.ForEach(config.groupItemSel, func(i int, f *colly.HTMLElement) {
				title := getTitle(cinema, f)
				if title == "" {
					return
				}

				// if groupDate != "" {
				// 	fmt.Println(groupDate)
				// }
				dateTimes := getDateTimes(cinema, f, groupDates[groupDateCounter])
				// dateTimes := getDateTimes(cinema, e, groupDate)
				// if len(dateTimes) == 0 {
				// 	return
				// }

				moviesMx.Lock()
				defer moviesMx.Unlock()
				for _, dateTime := range dateTimes {
					movies[title] = append(movies[title], showing{cinema, dateTime})
				}
			})
			groupDateCounter++
		})
	}

	c.OnHTML(config.rootSel, func(e *colly.HTMLElement) {
		title := getTitle(cinema, e)
		// if cinema == Kijow {
		// 	fmt.Println(title)
		// }
		if title == "" {
			return
		}

		// if groupDate != "" {
		// 	fmt.Println(groupDate)
		// }
		// proper error handling
		dateTimes := getDateTimes(cinema, e, "")
		// dateTimes := getDateTimes(cinema, e, groupDate)
		// if cinema == Kijow {
		// 	fmt.Println(dateTimes)
		// }
		if len(dateTimes) == 0 {
			return
		}

		moviesMx.Lock()
		defer moviesMx.Unlock()
		for _, dateTime := range dateTimes {
			// skip over showings from earlier in the day
			if dateTime.Before(time.Now().Local()) {
				continue
			}

			movies[title] = append(movies[title], showing{cinema, dateTime})
		}
	})

	if config.linkSel != "" {
		visited := map[string]struct{}{}
		c.OnHTML(config.linkSel, func(e *colly.HTMLElement) {
			linkText := e.DOM.Text()
			if _, present := visited[linkText]; !present {
				err := visitNext(cinema, e)
				if err == nil {
					visited[linkText] = struct{}{}
				}
			}
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

	// if cinema == PodBaranami {
	// 	fmt.Println(dateTimeWords)
	// 	fmt.Println([]int{1, 2})
	// }

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
		if month < time.Now().Month() {
			year++
		}
	}

	location := time.Now().Location()

	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, location)
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
		title = strings.TrimSpace(title)

	case Paradox:
		title = e.DOM.Find("a.item-title").Text()
		// fmt.Println(title)

	case Sfinks:
		title = e.DOM.Find("span.title").Text()
	}

	return title
}

func getDateTimes(cinema cinema, e *colly.HTMLElement, groupDate string) []time.Time {
	var dateTimeStrs = []string{}
	switch cinema {
	case Kika, Agrafka:
		dateRaw := e.DOM.Find("div.date").Text()
		dateLines := strings.Split(dateRaw, "\n")
		dateLines = dateLines[len(dateLines)-2:]
		dateRaw = strings.Join(dateLines, "")
		dateRaw = groupDate + " " + dateRaw
		dateTimeStrs = append(dateTimeStrs, dateRaw)

	case Kijow:
		dateRaw := e.DOM.Find("span.cd-date").Text()
		dateRaw = groupDate + " " + dateRaw
		// fmt.Println(dateRaw)
		dateTimeStrs = append(dateTimeStrs, dateRaw)

	case PodBaranami:
		timeRaw := e.DOM.Find("span").Find("a").Text()
		onclickStr, found := e.DOM.Find("span").Find("a").Attr("onclick")
		if !found {
			return []time.Time{}
		}
		onClickWords := strings.Split(onclickStr, ",")
		dateRaw := onClickWords[len(onClickWords)-5]
		dateRaw = groupDate + " " + dateRaw + " " + timeRaw
		dateTimeStrs = append(dateTimeStrs, dateRaw)

	case Paradox:
		dateRaw, _ := e.DOM.Attr("data-date")
		timeRaw := e.DOM.Find("div.item-time").Text()
		dateRaw = groupDate + " " + dateRaw + " " + timeRaw
		// fmt.Println(dateRaw)
		dateTimeStrs = append(dateTimeStrs, dateRaw)

	case Sfinks:
		dateTimeElement := e.DOM.Find("span.kali_data_od")
		dateRaw := dateTimeElement.Find("span").First().Text()
		timeRaw := dateTimeElement.Find("span").Eq(2).Text()
		dateRaw = groupDate + " " + dateRaw + " " + timeRaw
		dateTimeStrs = append(dateTimeStrs, dateRaw)
	}

	var dateTimes = make([]time.Time, len(dateTimeStrs))
	for i, e := range dateTimeStrs {
		dateTimes[i] = processDateTimeString(e, cinema)
	}
	return dateTimes
}

func visitNext(cinema cinema, e *colly.HTMLElement) error {
	var err error
	switch cinema {
	case Kijow:
		month := e.DOM.Find("span.daynumber:not(.active)").Text()
		if month != "" {
			link := e.Attr("href")
			// fmt.Println(link)
			err = e.Request.Visit(link)
			// fmt.Println(a)
		}

	case Sfinks:
		link := e.Attr("href")
		// fmt.Println(link)
		err = e.Request.Visit(link)
		// fmt.Println(a)
	}
	return err
}

// func redirect(req *http.Request, via []*http.Request) error {
// 	fmt.Println("REDIRECTEDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
// 	return colly.ErrAlreadyVisited
// }
