package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	_ "modernc.org/sqlite"
)

var excludedByKeywords = [...]string{"UKRAINIAN", "UKRAIŃSKI", "DLA OSÓB", "KLUB SENIORA", "DKF KROPKA DLA DZIECI"}

// TODO properly remove punctuation?
var removedKeywords = [...]string{"2D", "3D", "- DUBBING", "DUBBING", " - NAPISY", "NAPISY",
	"TANI WTOREK:", "DKF KROPKA:", "DKF PEŁNA SALA:",
	"- PRZEDPREMIERA", "+ ENG SUB", "- POKAZ SPECJALNY Z DYSKUSJĄ", "– WERSJA REŻYSERSKA",
	". WERSJA REŻYSERSKA", "- POKAZ SPECJALNY", "POKAZ SPECJALNY"}

func main() {
	originFlagPtr := flag.String("origin", "", "The Gotify origin \"scheme://authority\".")
	tokenFlagPtr := flag.String("token", "", "The Gotify token.")
	logFlagPtr := flag.Bool("log", false, "Determines if the result should be logged as a markdown file.")
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
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL
	);
	`
	_, err = db.Exec(sqlCreate)
	if err != nil {
		panic(err)
	}

	answerCountdown := len(cinemasToScrape) + len(cinemasToFetch)
	resultCh := make(chan result)

	scrape(resultCh)
	fetch(resultCh)

	var receivedArr = [LAST]bool{}
	movies := map[string][]showing{}

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
			}

		skipMovie:
		}

		answerCountdown -= 1
		if answerCountdown == 0 {
			break
		}
	}

	// sort showings per movie by datetime
	for _, movie := range movies {
		sort.Slice(movie, func(a, b int) bool {
			return movie[a].time.Before(movie[b].time)
		})
	}

	// split movies
	moviesToday := map[string][]showing{}
	moviesYesterday := map[string][]showing{}
	moviesLastWeek := map[string][]showing{}
	moviesRest := map[string][]showing{}
	for title, showings := range movies {
		sqlSelect := `
			SELECT first_seen, last_seen
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
			// no db entry -> add it and treat it as a new movie from today
			moviesToday[title] = showings

			sqlInsert := `
				INSERT INTO movies
					(title, original_title, first_seen, last_seen)
					VALUES(?, ?, ?, ?);
			`
			_, err = db.Exec(sqlInsert, title, "", todayStr, todayStr)
			if err != nil {
				panic(err)
			}
		} else {
			var firstSeenStr, lastSeenStr string
			err := rows.Scan(&firstSeenStr, &lastSeenStr)
			if err != nil {
				panic(err)
			}
			rows.Close()

			lastSeen, _ := time.Parse(time.DateOnly, lastSeenStr)
			today, _ := time.Parse(time.DateOnly, todayStr)
			hourDiff := today.Sub(lastSeen).Hours()

			if hourDiff > 25 {
				// haven't appeared in any repertoires in a while -> treat it as
				// a 'new' movie from today
				moviesToday[title] = showings

				sqlUpdate := `
					UPDATE movies
						SET first_seen = ?, last_seen = ?
						WHERE title = ?;
				`
				_, err = db.Exec(sqlUpdate, todayStr, todayStr, title)
				if err != nil {
					panic(err)
				}
			} else {
				// otherwise determine how long has it been since
				// it's been added to the currect repertoire aggregate
				firstSeen, _ := time.Parse(time.DateOnly, firstSeenStr)
				hourDiff = today.Sub(firstSeen).Hours()

				if hourDiff <= 50 {
					moviesYesterday[title] = showings
				} else if hourDiff <= 170 {
					moviesLastWeek[title] = showings
				} else {
					moviesRest[title] = showings
				}

				sqlUpdate := `
					UPDATE movies
						SET last_seen = ?
						WHERE title = ?;
				`
				_, err = db.Exec(sqlUpdate, todayStr, title)
				if err != nil {
					panic(err)
				}
			}
		}
	}

	var sb strings.Builder
	// gotify android app markdown renderer needs '\n' for a newline it seems
	if len(moviesToday) > 0 {
		sb.WriteString(`# **TODAY**  \n`)
		writeMovies(&sb, moviesToday)
	}

	if len(moviesYesterday) > 0 {
		sb.WriteString(`# **YESTERDAY**  \n`)
		writeMovies(&sb, moviesYesterday)
	}

	if len(moviesLastWeek) > 0 {
		sb.WriteString(`# **LAST WEEK**  \n`)
		writeMovies(&sb, moviesLastWeek)
	}

	if len(moviesRest) > 0 {
		sb.WriteString(`# **THE REST**  \n`)
		writeMovies(&sb, moviesRest)
	}

	totalLine := fmt.Sprintf(`**TOTAL: %d**  \n`, len(movies))
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

	summary := sb.String()

	if *originFlagPtr != "" && *tokenFlagPtr != "" {
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

		gotifyUrl := fmt.Sprintf("%s/message?token=%s", *originFlagPtr, *tokenFlagPtr)

		http.Post(gotifyUrl, "application/json", reqBody)
	}

	if *logFlagPtr {
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
		titleLine := fmt.Sprintf(`## %s`, title)
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
					fmt.Sprintf(`  \n======**%02d/%02d/%d**======  \n`,
						dateTime.Day(), dateTime.Month(), dateTime.Year())
				sb.WriteString(dateLine)
				lastDate = date
			}

			showingLine :=
				fmt.Sprintf(`%s  %02d:%02d  \n`,
					showing.cinema.String(), dateTime.Hour(), dateTime.Minute())
			sb.WriteString(showingLine)
		}

		sb.WriteString(`  \n`)
	}
}
