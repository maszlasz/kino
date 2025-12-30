package main

import (
	"strings"
	"time"

	"github.com/gocolly/colly"
)

type scrapeSite struct {
	cinema  cinema
	rootSel string
	linkSel string
	charSet string
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

var cinemasToScrape = [...]scrapeSite{
	{cinema: Agrafka,
		rootSel: "div.repertoire-once"},
	{cinema: Kijow,
		rootSel: "div.cd-timeline-block",
		linkSel: "a[href].eventcard.col-6"},
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
		rootSel: "span.zajawka",
		linkSel: "a[href][title^='Strona']"},
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

		if !dateTime.Before(time.Now().Local()) {
			movies[title] = append(movies[title], showing{cinema, dateTime})
		}
	})

	if site.linkSel != "" {
		c.OnHTML(site.linkSel, func(e *colly.HTMLElement) {
			link := getNextUrl(cinema, e)
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
