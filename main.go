package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly"
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
	CityBonarka: "https://www.cinema-city.pl/kina/bonarka/1090#/buy-tickets-by-cinema?in-cinema=1090",
	Kijow:       "https://kupbilet.kijow.pl/MSI/mvc/?sort=Date",
	Kika:        "https://bilety.kinokika.pl",
	Mikro:       "https://kinomikro.pl/repertoire/?view=all",
	Sfinks:      "https://kinosfinks.okn.edu.pl/wydarzenia.html"}

type showing struct {
	cinema cinema
	time   time.Time
}

type siteConfig struct {
	rootSel     string
	titleSel    string
	datetimeSel string
	linkSel     string
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
var excludedByKeywords = [...]string{"UKRAINIAN DUBBING"}
var removedKeywords = [...]string{"2D", "3D", "DUBBING"}
var timeRegex = regexp.MustCompile("^(([0-1]?[0-9])|(2[0-3]))(:[0-5][0-9])+$")

func main() {
	cinemasToCheck := [...]site{
		{Kika,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Agrafka,
			siteConfig{
				rootSel: "div.repertoire-once"}},
		{Kijow,
			siteConfig{
				rootSel: "div.cd-timeline-block",
				linkSel: "a[href].eventcard.col-6"}}}

	// answerCountdown := len(cinemasToCheck)
	answerCountdown := len(cinemasToCheck) + 1
	resultCh := make(chan result)

	for _, cinema := range cinemasToCheck {
		// timeouts
		go scrapeCinema(cinema, resultCh)
	}
	go parseMultikino(Multikino, resultCh)

	var receivedArr = [LAST]bool{}
	movies := map[string][]showing{}
	titles := []string{}

	for {
		result := <-resultCh
		receivedArr[result.cinema] = true

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

	slices.Sort(titles)
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
	req, _ := http.NewRequest("GET", "https://www.multikino.pl/api/microservice", nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
	}
	res.Body.Close()

	req, _ = http.NewRequest("GET", "https://www.multikino.pl/api/microservice/showings/cinemas/0005/films", nil)
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
				time := processDateTimeString(timeString)
				showings = append(showings, showing{cinema: Multikino, time: time})
			}
		}
		movies[title] = showings
	}

	resultCh <- result{cinema: Multikino, movies: movies}
}

func scrapeCinema(site site, resultCh chan result) {
	cinema := site.cinema
	config := site.config

	c := colly.NewCollector(
		colly.MaxDepth(1),
	)
	c.RedirectHandler = redirect

	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Site: ", r.URL.String())
	})

	movies := make(map[string][]showing)
	var moviesMx sync.Mutex

	c.OnHTML(config.rootSel, func(e *colly.HTMLElement) {
		title := getTitle(cinema, e)
		if title == "" {
			return
		}

		dateTimes := getDateTimes(cinema, e)
		if len(dateTimes) == 0 {
			return
		}

		moviesMx.Lock()
		defer moviesMx.Unlock()
		for _, dateTime := range dateTimes {
			movies[title] = append(movies[title], showing{cinema, dateTime})
		}
	})

	if config.linkSel != "" {
		c.OnHTML(config.linkSel, func(e *colly.HTMLElement) {
			visitNext(cinema, e)
		})
	}

	c.Visit(repertoires[cinema])
	c.Wait()

	resultCh <- result{cinema: cinema, movies: movies}
}

func processDateTimeString(rawDateTime string) time.Time {
	dateTimeWords := strings.Fields(rawDateTime)
	if len(dateTimeWords) == 1 {
		dateTimeWords = strings.FieldsFunc(
			dateTimeWords[0],
			func(r rune) bool {
				return r == 'T' || r == '-'
			})
	}

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
			} else if year == 0 {
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
	}

	return title
}

func getDateTimes(cinema cinema, e *colly.HTMLElement) []time.Time {
	var dateTimeStrs = []string{}
	switch cinema {
	case Kika, Agrafka:
		dateRaw := e.DOM.Find("div.date").Text()
		dateLines := strings.Split(dateRaw, "\n")
		dateLines = dateLines[len(dateLines)-2:]
		dateRaw = strings.Join(dateLines, "")
		dateTimeStrs = append(dateTimeStrs, dateRaw)

	case Kijow:
		dateRaw := e.DOM.Find("span.cd-date").Text()
		dateTimeStrs = append(dateTimeStrs, dateRaw)
	}

	var dateTimes = make([]time.Time, len(dateTimeStrs))
	for i, e := range dateTimeStrs {
		dateTimes[i] = processDateTimeString(e)
	}
	return dateTimes
}

func visitNext(cinema cinema, e *colly.HTMLElement) {
	switch cinema {
	case Kijow:
		month := e.DOM.Find("span.daynumber:not(.active)").Text()
		if month != "" {
			link := e.Attr("href")
			e.Request.Visit(link)
		}

	default:
		return
	}
}

func redirect(req *http.Request, via []*http.Request) error {
	fmt.Println("REDIRECTEDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
	return colly.ErrAlreadyVisited
}
