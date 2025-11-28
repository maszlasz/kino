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
	Kika:    "https://bilety.kinokika.pl",
	Agrafka: "https://bilety.kinoagrafka.pl"}

type showing struct {
	cinema cinema
	time   time.Time
}

type result struct {
	cinema cinema
	movies map[string][]showing
}

// var moviesMx sync.Mutex
// var wg sync.WaitGroup

var timeRegex = regexp.MustCompile("^(([0-1]?[0-9])|(2[0-3]))(:[0-5][0-9])+$")

func main() {
	cinemasToCheck := []cinema{Kika, Agrafka}
	answerCountdown := len(cinemasToCheck) + 1
	resultCh := make(chan result)

	for _, cinema := range cinemasToCheck {
		go parseCinemaKikaAgrafka(cinema, resultCh)
	}
	go parseMultikino(Multikino, resultCh)

	var receivedArr = [LAST]bool{}
	movies := map[string][]showing{}
	titles := []string{}

	for {
		result := <-resultCh
		receivedArr[result.cinema] = true

		for k, v := range result.movies {
			title := strings.ToUpper(strings.TrimSpace(k))
			if showings, ok := movies[title]; ok {
				movies[title] = append(showings, v...)
			} else {
				movies[title] = v
				titles = append(titles, title)
			}
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

	fmt.Printf("TOTAL: %x \n", len(titles))
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

func parseCinemaKikaAgrafka(cinema cinema, resultCh chan result) {
	c := colly.NewCollector()
	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Site: ", r.URL.String())
	})

	movies := make(map[string][]showing)
	var showing showing
	showing.cinema = cinema

	c.OnHTML("div.repertoire-once", func(e *colly.HTMLElement) {
		title := e.DOM.Find("div[class*=title]").Find("a").Text()
		if title == "" {
			return
		}

		dateRaw := e.DOM.Find("div[class*=date]").Text()
		dateLines := strings.Split(dateRaw, "\n")
		dateLines = dateLines[len(dateLines)-2:]
		dateRaw = strings.Join(dateLines, "")
		showing.time = processDateTimeString(dateRaw)

		movies[title] = append(movies[title], showing)
	})

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
			month = mapMonthName(dateTime)
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

func mapMonthName(month string) time.Month {
	months := map[string]time.Month{
		"sty": time.January,
		"lut": time.February,
		"mar": time.March,
		"kwi": time.April,
		"maj": time.May,
		"cze": time.June,
		"lip": time.July,
		"sie": time.August,
		"wrz": time.September,
		"paz": time.October,
		"paź": time.October,
		"lis": time.November,
		"gru": time.December,
	}

	return months[strings.ToLower(month[:3])]
}
