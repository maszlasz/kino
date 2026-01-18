package main

import (
	"strings"
	"time"

	"github.com/gocolly/colly"
)

type scrapeSite struct {
	cinema      cinema
	rootSel     string
	nextPageSel string
	charSet     string
}

var repertoires = map[cinema]string{
	Agrafka:     "https://bilety.kinoagrafka.pl/",
	Kijow:       "https://kupbilet.kijow.pl/MSI/mvc/pl?sort=Date&date=1970-01&datestart=0/",
	Kika:        "https://bilety.kinokika.pl/",
	Mikro:       "https://kinomikro.pl/repertoire/?view=all/",
	Paradox:     "https://kinoparadox.pl/repertuar/",
	PodBaranami: "https://kinopodbaranami.pl/repertuar.php/",
	Sfinks:      "https://kinosfinks.okn.edu.pl/wydarzenia-szukaj-strona-1.html", //can't have a slash at the end of the url for some reason
}

var ticketBases = map[cinema]string{
	Agrafka:     "https://bilety.kinoagrafka.pl/",
	Kijow:       "https://kupbilet.kijow.pl/",
	Kika:        "https://bilety.kinokika.pl/",
	Mikro:       "https://kinomikro.pl/",
	Paradox:     "https://kinoparadox.pl/repertuar/",
	PodBaranami: "https://www.kinopodbaranami.pl/",
	Sfinks:      "https://kinosfinks.okn.edu.pl/",
}

var cinemasToScrape = [...]scrapeSite{
	{cinema: Agrafka,
		rootSel: "div.repertoire-once"},
	{cinema: Kijow,
		rootSel:     "div.cd-timeline-block",
		nextPageSel: "a[href].eventcard.col-6"},
	{cinema: Kika,
		rootSel: "div.repertoire-once"},
	{cinema: Paradox,
		rootSel: "div.list-item__content__row"},
	{cinema: Mikro,
		rootSel: "section.row"},
	{cinema: PodBaranami,
		rootSel: "li[title]",
		charSet: "iso-8859-2"},
	{cinema: Sfinks,
		rootSel:     "span.zajawka",
		nextPageSel: "a[href][title^='Strona']"},
}

func scrape(resultCh chan result) {
	for _, cinema := range cinemasToScrape {
		go scrapeCinema(cinema, resultCh)
	}
}

func scrapeCinema(site scrapeSite, resultCh chan result) {
	cinema := site.cinema

	c := colly.NewCollector(
		colly.MaxDepth(2),
		colly.Async(true),
	)

	c.OnRequest(func(r *colly.Request) {
		if site.charSet != "" {
			r.ResponseCharacterEncoding = site.charSet
		}
	})

	movies := make(map[string][]showing)

	var lastDate string
	c.OnHTML(site.rootSel, func(e *colly.HTMLElement) {
		title := getTitle(cinema, e)
		if title == "" {
			return
		}

		dateTime := getDateTime(cinema, e, &lastDate)

		url := getShowingUrl(cinema, e)

		if !dateTime.Before(time.Now().Local()) {
			movies[title] = append(movies[title], showing{cinema, dateTime, url})
		}
	})

	if site.nextPageSel != "" {
		c.OnHTML(site.nextPageSel, func(e *colly.HTMLElement) {
			link := getNextNextPageUrl(cinema, e)
			c.Visit(link)
		})
	}

	c.Visit(repertoires[cinema])
	c.Wait()

	resultCh <- result{cinema: cinema, movies: movies}
}

func getTitle(cinema cinema, e *colly.HTMLElement) string {
	var title string

	switch cinema {
	case Agrafka, Kika:
		title = e.DOM.Find("a").First().Text()

	case Kijow:
		title = e.DOM.Find("h2").After("i").Text()

	case Mikro:
		title = e.DOM.Find("a.repertoire-item-title").Text()

	case Paradox:
		title = e.DOM.Find("a.item-title").Text()

	case PodBaranami:
		title = e.DOM.Find("a").First().Text()
		// lots of newlines and garbage around it
		title = strings.TrimSpace(title)

	case Sfinks:
		title = e.DOM.Find("span.title").Text()
	}

	return title
}

func getDateTime(cinema cinema, e *colly.HTMLElement, lastDate *string) time.Time {
	var dateTimeStr string

	switch cinema {
	case Agrafka, Kika:
		dateRaw := e.DOM.Find("div.date").Text()
		dateLines := strings.Split(dateRaw, "\n")
		dateLines = dateLines[len(dateLines)-2:]
		dateTimeStr = strings.Join(dateLines, "")

	case Kijow:
		dateTimeStr = e.DOM.Find("span.cd-date").Text()

	case Mikro:
		dateElementMaybe := e.DOM.Find("div.repertoire-separator")
		if dateElementMaybe.Length() != 0 {
			*lastDate = dateElementMaybe.Text()
		}
		dateRaw := *lastDate
		timeRaw := e.DOM.Find("p.repertoire-item-hour").Text()
		dateTimeStr = dateRaw + " " + timeRaw

	case Paradox:
		dateRaw, _ := e.DOM.Attr("data-date")
		timeRaw := e.DOM.Find("div.item-time").Text()
		dateTimeStr = dateRaw + " " + timeRaw

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

	case Sfinks:
		dateTimeElement := e.DOM.Find("span.kali_data_od")
		dateRaw := dateTimeElement.Find("span").First().Text()
		timeRaw := dateTimeElement.Find("span").Eq(2).Text()
		dateTimeStr = dateRaw + " " + timeRaw
	}

	return processDateTimeString(dateTimeStr, cinema)
}

func getShowingUrl(cinema cinema, e *colly.HTMLElement) string {
	var url string
	var exists bool

	switch cinema {
	case Agrafka:
		url, exists = e.DOM.Find("a.button").Attr("href")
		if exists {
			url = ticketBases[Agrafka] + url
		}

	case Kika:
		url, exists = e.DOM.Find("a.button").Attr("href")
		if exists {
			url = ticketBases[Kika] + url
		}

	case Kijow:
		url, exists = e.DOM.Find("a.btn-badge2").Attr("href")
		if exists {
			url = ticketBases[Kijow] + url
		}

	case Mikro:
		url, exists = e.DOM.Find("a.repertoire-item-button").Attr("href")
		if exists {
			url = ticketBases[Mikro] + url
		}

	case Paradox:
		url, exists = e.DOM.Find("a.btn").Attr("href")

	case PodBaranami:
		url, exists = e.DOM.Find("a[onclick]").Attr("href")
		if exists {
			url = ticketBases[PodBaranami] + url
		}

	case Sfinks:
		url, exists = e.DOM.Find("a").Attr("href")
		if exists {
			url = ticketBases[Sfinks] + url
		}
	}

	if !exists {
		return ""
	}

	return url
}

func getNextNextPageUrl(cinema cinema, e *colly.HTMLElement) string {
	var url string

	switch cinema {
	case Kijow, Sfinks:
		url = e.Request.AbsoluteURL(e.Attr("href"))
	}

	return url
}
