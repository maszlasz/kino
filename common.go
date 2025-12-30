package main

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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

type result struct {
	cinema cinema
	movies map[string][]showing
}

var timeRegex = regexp.MustCompile("^(([0-1]?[0-9])|(2[0-3]))(:[0-5][0-9])+$")

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
