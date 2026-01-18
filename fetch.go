package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"time"
)

type fetchSite struct {
	cinema     cinema
	processFun func(cinema, chan result)
}

var cinemaApiIds = map[cinema]string{
	Multikino:       "0005",
	CCityBonarka:    "1090",
	CCityKazimierz:  "1076",
	CCityZakopianka: "1064",
}

var apiUrls = map[string]string{
	"MultikinoBase":       "https://multikino.pl/",
	"MultikinoCookies":    "https://multikino.pl/api/microservice/",
	"MultikinoFilmsStart": "https://multikino.pl/api/microservice/showings/cinemas/",
	"MultikinoFilmsEnd":   "/films/",
	"CCityDatesStart":     "https://cinema-city.pl/pl/data-api-service/v1/quickbook/10103/dates/in-cinema/",
	"CCityDatesEnd":       "/until/",
	"CCityFilmsStart":     "https://cinema-city.pl/pl/data-api-service/v1/quickbook/10103/film-events/in-cinema/",
	"CCityFilmsEnd":       "/at-date/",
}

var cinemasToFetch = [...]fetchSite{
	{cinema: Multikino,
		processFun: fetchMultikino},
	{cinema: CCityBonarka,
		processFun: fetchCCity},
	{cinema: CCityKazimierz,
		processFun: fetchCCity},
	{cinema: CCityZakopianka,
		processFun: fetchCCity},
}

func fetch(resultCh chan result) {
	for _, cinema := range cinemasToFetch {
		go cinema.processFun(cinema.cinema, resultCh)
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
				url := apiUrls["MultikinoBase"] + sessionJson["bookingUrl"].(string)
				showings = append(showings, showing{cinema, time, url})
			}
		}
		movies[title] = showings
	}

	resultCh <- result{cinema: cinema, movies: movies}
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
	moviesPath := moviesBasePath + date
	req, _ := http.NewRequest("GET", moviesPath, nil)
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

		url := eventMap["bookingLink"].(string)

		event := showing{cinema, dateTime, url}
		if showings, ok := movies["title"]; ok {
			movies[title] = append(showings, event)
		} else {
			movies[title] = []showing{event}
		}
	}

	moviesCh <- movies
}
