package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	_ "modernc.org/sqlite"
)

var excludedByKeywords = [...]string{
	"UKRAINIAN", "UKRAIŃSKI",
	"DLA OSÓB", "KLUB SENIORA", "DKF KROPKA DLA DZIECI",
}

// TODO properly remove punctuation?
var removedKeywords = [...]string{
	"2D", "3D", "DUBBING PL", "DUBBING", "NAPISY",
	"TANI WTOREK", "DKF KROPKA", "DKF PEŁNA SALA",
	"PRZEDPREMIERA", "ENG SUB", "POKAZ SPECJALNY Z DYSKUSJĄ",
	"WERSJA REŻYSERSKA", "POKAZ SPECJALNY",
	"POKAZ PRZEDPREMIEROWY", "WERSJA ORYGINALNA",
	"NAJLEPSZE Z NAJGORSZYCH",
}

var allPunctuationRegex = regexp.MustCompile(`\p{P}`)
var multipleSpacesRegex = regexp.MustCompile(`[\s\p{Zs}]{2,}`)

// TODO enums etc.
var filmwebUrls = map[string]string{
	"SearchStart":  "https://www.filmweb.pl/api/v1/search?query=",
	"SearchEnd":    "&pageSize=1",
	"PreviewStart": "https://www.filmweb.pl/api/v1/film/",
	"PreviewEnd":   "/preview",
	"FilmStart":    "https://www.filmweb.pl/film/",
}

type timePeriod int

const (
	Today timePeriod = iota
	Yesterday
	LastWeek
	Earlier
)

type movieInfo struct {
	secondaryTitle string
	filmwebId      string
	showings       []showing
}

func main() {
	originFlagPtr := flag.String("gotify-origin", "", "The Gotify origin \"scheme://authority\".")
	gotifyTokenFlagPtr := flag.String("gotify-token", "", "The Gotify token.")
	logFlagPtr := flag.Bool("log", false, "Determines if the result should be logged as a markdown file.")
	flag.Parse()

	dbPtr, err := sql.Open("sqlite", "./movies.db")
	if err != nil {
		panic(err)
	}
	defer dbPtr.Close()

	_, err = dbPtr.Exec("PRAGMA journal_mode=WAL;")
	if err != nil {
		panic(err)
	}
	// TODO temporary solution for DB access
	_, err = dbPtr.Exec("PRAGMA busy_timeout=50000;")
	if err != nil {
		panic(err)
	}
	dbPtr.SetMaxOpenConns(1)

	sqlCreate := `
	CREATE TABLE IF NOT EXISTS movies (
		title TEXT NOT NULL PRIMARY KEY,
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		secondary_title TEXT,
		ext_db_id TEXT
	);
	`
	_, err = dbPtr.Exec(sqlCreate)
	if err != nil {
		panic(err)
	}

	answerCountdown := len(cinemasToScrape) + len(cinemasToFetch)
	resultCh := make(chan result)

	scrape(resultCh)
	fetch(resultCh)

	var receivedArr = [CINEMA_COUNT]bool{}
	titleToShowings := map[string][]showing{}

WaitForCinemas:
	for {
		var result result
		select {
		case result = <-resultCh:

		case <-time.After(120 * time.Second):
			break WaitForCinemas
		}

		receivedArr[result.cinema] = true
		var lenT int

		for rawTitle, showings := range result.titleToShowings {
			title := strings.ToUpper(rawTitle)

			for _, kw := range excludedByKeywords {
				if strings.Contains(title, kw) {
					goto skipMovie
				}
			}

			title = allPunctuationRegex.ReplaceAllString(title, " ")

			for _, kw := range removedKeywords {
				title = strings.ReplaceAll(title, kw, "")
			}

			title = multipleSpacesRegex.ReplaceAllString(title, " ")

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

			if showingsOld, ok := titleToShowings[title]; ok {
				titleToShowings[title] = append(showingsOld, showings...)
			} else {
				titleToShowings[title] = showings
			}

		skipMovie:
		}

		answerCountdown -= 1
		if answerCountdown == 0 {
			break WaitForCinemas
		}
	}

	// sort showings per movie by datetime
	for _, showings := range titleToShowings {
		sort.Slice(showings, func(a, b int) bool {
			return showings[a].time.Before(showings[b].time)
		})
	}

	periodToMovie := updateDbGetPeriodAggregate(titleToShowings, dbPtr)

	summary := createSummary(periodToMovie, receivedArr)

	if *originFlagPtr != "" && *gotifyTokenFlagPtr != "" {
		postSummaryToGotify(summary, *originFlagPtr, *gotifyTokenFlagPtr)
	}

	if *logFlagPtr {
		logSummary(summary)
	}
}

func updateDbGetPeriodAggregate(titleToShowings map[string][]showing, dbPtr *sql.DB) map[timePeriod]map[string]*movieInfo {
	periodToMovie := map[timePeriod]map[string]*movieInfo{}
	periodToMovie[Today] = map[string]*movieInfo{}
	periodToMovie[Yesterday] = map[string]*movieInfo{}
	periodToMovie[LastWeek] = map[string]*movieInfo{}
	periodToMovie[Earlier] = map[string]*movieInfo{}

	var queryUpdateWg sync.WaitGroup

	client := &http.Client{}

	for title, showings := range titleToShowings {
		sqlSelect := `
			SELECT secondary_title, ext_db_id, first_seen, last_seen
				FROM movies
				WHERE title = ?;
		`
		rows, err := dbPtr.Query(sqlSelect, title)
		if err != nil {
			panic(err)
		}

		movieInfoPtr := &movieInfo{showings: showings}

		today := time.Now()
		todayStr :=
			fmt.Sprintf("%d-%02d-%02d",
				today.Year(), today.Month(), today.Day())

		if !rows.Next() {
			// no db entry -> add it and treat it as a new movie from today
			periodToMovie[Today][title] = movieInfoPtr

			sqlInsert := `
				INSERT INTO movies
					(title, first_seen, last_seen)
					VALUES(?, ?, ?);
			`
			_, err = dbPtr.Exec(sqlInsert, title, todayStr, todayStr)
			if err != nil {
				panic(err)
			}

			queryUpdateWg.Go(func() {
				searchAndUpdateMovie(title, client, movieInfoPtr, dbPtr)
			})
		} else {
			var firstSeenStr, lastSeenStr string
			var secondaryTitle, filmwebId sql.NullString
			err :=
				rows.Scan(&secondaryTitle, &filmwebId, &firstSeenStr, &lastSeenStr)

			if err != nil {
				panic(err)
			}
			rows.Close()

			lastSeen, _ := time.Parse(time.DateOnly, lastSeenStr)
			today, _ := time.Parse(time.DateOnly, todayStr)
			lastSeenHourDiff := today.Sub(lastSeen).Hours()

			// later will be overridden by a goroutine call to external movie db if needed
			movieInfoPtr.secondaryTitle = secondaryTitle.String
			movieInfoPtr.filmwebId = filmwebId.String

			if lastSeenHourDiff > 25 {
				// haven't appeared in any repertoires in a while -> treat it as
				// a 'new' movie from today - all assuming it's ran daily

				periodToMovie[Today][title] = movieInfoPtr

				sqlUpdate := `
					UPDATE movies
						SET first_seen = ?, last_seen = ?
						WHERE title = ?;
				`
				_, err = dbPtr.Exec(sqlUpdate, todayStr, todayStr, title)
				if err != nil {
					panic(err)
				}
			} else {
				// otherwise determine how long has it been since
				// it's been added to the currect repertoire aggregate
				firstSeen, _ := time.Parse(time.DateOnly, firstSeenStr)
				hourDiff := today.Sub(firstSeen).Hours()

				var period timePeriod
				if hourDiff <= 50 {
					period = Yesterday
				} else if hourDiff <= 170 {
					period = LastWeek
				} else {
					period = Earlier
				}
				periodToMovie[period][title] = movieInfoPtr

				sqlUpdate := `
					UPDATE movies
						SET last_seen = ?
						WHERE title = ?;
				`
				_, err = dbPtr.Exec(sqlUpdate, todayStr, title)
				if err != nil {
					panic(err)
				}
			}

			if !secondaryTitle.Valid {
				queryUpdateWg.Go(func() {
					searchAndUpdateMovie(title, client, movieInfoPtr, dbPtr)
				})
			}
		}
	}

	queryUpdateWg.Wait()

	return periodToMovie
}

func createSummary(periodToMovie map[timePeriod]map[string]*movieInfo, receivedArr [CINEMA_COUNT]bool) string {
	var sb strings.Builder

	// gotify android app markdown renderer needs '\n' for a newline it seems
	if len(periodToMovie[Today]) > 0 {
		sb.WriteString(`# **TODAY**  \n`)
		writeMovies(&sb, periodToMovie[Today])
	}

	if len(periodToMovie[Yesterday]) > 0 {
		sb.WriteString(`# **YESTERDAY**  \n`)
		writeMovies(&sb, periodToMovie[Yesterday])
	}

	if len(periodToMovie[LastWeek]) > 0 {
		sb.WriteString(`# **LAST WEEK**  \n`)
		writeMovies(&sb, periodToMovie[LastWeek])
	}

	if len(periodToMovie[Earlier]) > 0 {
		sb.WriteString(`# **EARLIER**  \n`)
		writeMovies(&sb, periodToMovie[Earlier])
	}

	totalCount := 0
	for _, movieMap := range periodToMovie {
		totalCount += len(movieMap)
	}

	totalLine := fmt.Sprintf(`**TOTAL: %d**  \n`, totalCount)
	sb.WriteString(totalLine)

	if slices.Contains(receivedArr[:], false) {
		sb.WriteString(`RESULTS NOT RECEIVED FROM:  \n`)
		for cinemaIndex, received := range receivedArr {
			if !received {
				cinemaLine := fmt.Sprintf(`%s  \n`, cinema(cinemaIndex))
				sb.WriteString(cinemaLine)
			}
		}
	}

	return sb.String()
}

func postSummaryToGotify(summary string, origin string, token string) {
	today := time.Now()
	todayStr :=
		fmt.Sprintf("%02d/%02d/%d",
			today.Day(), today.Month(), today.Year())

	reqBodyStr := fmt.Sprintf(`{
			"title": "%s",
			"message": "%s",
			"extras": {
				"client::display": {
					"contentType": "text/markdown"
				}
			}
		}`, todayStr, summary)
	reqBody := strings.NewReader(reqBodyStr)

	gotifyUrl := fmt.Sprintf("%s/message?token=%s", origin, token)

	http.Post(gotifyUrl, "application/json", reqBody)
}

func logSummary(summary string) {
	today := time.Now()
	todayStr :=
		fmt.Sprintf("%d-%02d-%02d",
			today.Year(), today.Month(), today.Day())

	filepath := fmt.Sprintf("%s.md", todayStr)
	file, err := os.Create(filepath)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	summary = strings.ReplaceAll(summary, `\n`, `<br>`)
	file.WriteString(summary)
}

func searchAndUpdateMovie(title string, client *http.Client, movieInfoPtr *movieInfo, dbPtr *sql.DB) {
	titleQuery := url.QueryEscape(title)
	url := filmwebUrls["SearchStart"] + titleQuery + filmwebUrls["SearchEnd"]
	req, _ := http.NewRequest("GET", url, nil)

	res, err := client.Do(req)
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

	searchHits := body["searchHits"].([]any)
	if len(searchHits) == 0 {
		return
	}
	searchHit := searchHits[0].(map[string]any)

	hitType := searchHit["type"].(string)
	if hitType != "film" {
		return
	}

	id := strconv.Itoa(int(searchHit["id"].(float64)))

	url = filmwebUrls["PreviewStart"] + id + filmwebUrls["PreviewEnd"]
	req, _ = http.NewRequest("GET", url, nil)

	res, err = client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}
	defer res.Body.Close()

	bodyBytes, _ = io.ReadAll(res.Body)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		log.Println(err)
		return
	}

	var filmwebTitle string
	secondaryTitle := ""
	if titleRaw, exists := body["title"]; exists {
		titleMap := titleRaw.(map[string]any)
		filmwebTitle = titleMap["title"].(string)

		if internationalTitleRaw, exists := body["internationalTitle"]; exists {
			titleMap := internationalTitleRaw.(map[string]any)
			secondaryTitle = titleMap["title"].(string)
		} else if originalTitleRaw, exists := body["originalTitle"]; exists {
			titleMap := originalTitleRaw.(map[string]any)
			secondaryTitle = titleMap["title"].(string)
		}
	} else {
		// polish movies usually only have originalTitle it seems
		titleMap := body["originalTitle"].(map[string]any)
		filmwebTitle = titleMap["title"].(string)
	}

	year := strconv.Itoa(int(body["year"].(float64)))

	movieInfoPtr.secondaryTitle = secondaryTitle
	movieInfoPtr.filmwebId =
		createFullFilmwebId(filmwebTitle, year, id)

	sqlUpdate := `
			UPDATE movies
				SET secondary_title = ?, ext_db_id = ?
				WHERE title = ?;
		`
	_, err =
		dbPtr.Exec(sqlUpdate, movieInfoPtr.secondaryTitle,
			movieInfoPtr.filmwebId, title)

	if err != nil {
		panic(err)
	}

}

// Based on the following JS funs from filmweb.pl,
// where type is assumed to be "film":
//
//	function u({type: e, title: r, year: n, id: i}) {
//	    return `${t()}/${e}/${s(r)}-${n}-${i}`
//	}
//
//	function s(e="") {
//	    return encodeURIComponent(e.replace(/[?!;/#\s]/g, " ").trim()).replace(/'/g, "%27").replace(/\+/g, "%2B").
//			        replace(/\(/g, "%28").replace(/\)/g, "%29").replace(/%20/g, "+").replace(/\+{2,}/g, "+")
//	}
func createFullFilmwebId(rawTitle string, year string, id string) string {
	title := url.QueryEscape(rawTitle)
	return fmt.Sprintf("%s-%s-%s", title, year, id)
}

func writeMovies(sb *strings.Builder, titleMap map[string]*movieInfo) {
	titles := make([]string, len(titleMap))
	i := 0
	for title := range titleMap {
		titles[i] = title
		i++
	}
	collator := collate.New(language.Polish)
	collator.SortStrings(titles)

	var lastDate time.Time
	for _, title := range titles {
		titleFormatted := strings.Replace(title, "\"", "\\\"", -1)

		if titleMap[title].filmwebId != "" {
			movieUrl := filmwebUrls["FilmStart"] + titleMap[title].filmwebId
			titleLine := fmt.Sprintf(`## [%s](%s)`, titleFormatted, movieUrl)
			sb.WriteString(titleLine)

			if titleMap[title].secondaryTitle != "" {
				originalTitleLine := fmt.Sprintf(`\n%s`, titleMap[title].secondaryTitle)
				sb.WriteString(originalTitleLine)
			}
		} else {
			titleLine := fmt.Sprintf(`## %s`, titleFormatted)
			sb.WriteString(titleLine)
		}

		lastDate = time.Time{}
		for _, showing := range titleMap[title].showings {
			dateTime := showing.time
			date :=
				time.Date(dateTime.Year(), dateTime.Month(), dateTime.Day(),
					0, 0, 0, 0,
					dateTime.Location())

			if !lastDate.Equal(date) {
				dateLine :=
					fmt.Sprintf(`  \n======**%02d/%02d/%d**======  \n`,
						dateTime.Day(), dateTime.Month(), dateTime.Year())
				sb.WriteString(dateLine)
				lastDate = date
			}

			showingLine :=
				fmt.Sprintf(`[%s](%s)  [%02d:%02d](%s)  \n`,
					showing.cinema.String(),
					websites[showing.cinema],
					dateTime.Hour(),
					dateTime.Minute(),
					showing.url)
			sb.WriteString(showingLine)
		}

		sb.WriteString(`  \n`)
	}
}
