package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net/http"
	"net/url"
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

	totalLine := fmt.Sprintf("TOTAL: %d \n", len(movies))
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

	summary := sb.String()

	// POST to gotify?
	if *originFlagPtr != "" && *tokenFlagPtr != "" {
		uv := url.Values{}
		today := time.Now()
		todayStr :=
			fmt.Sprintf("%02d/%02d/%d\n",
				today.Day(), today.Month(), today.Year())
		uv.Set("title", todayStr)
		uv.Add("message", summary)

		gotifyUrl := fmt.Sprintf("%s/message?token=%s", *originFlagPtr, *tokenFlagPtr)
		http.PostForm(gotifyUrl, uv)
	}

	if *logFlagPtr {
		today := time.Now()
		todayStr :=
			fmt.Sprintf("%d-%02d-%02d",
				today.Year(), today.Month(), today.Day())

		filepath := fmt.Sprintf("%s.log", todayStr)
		file, err := os.Create(filepath)
		if err != nil {
			panic(err)
		}
		defer file.Close()

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
