package tui

import "time"

func displayZone(t time.Time) string {
	name, offset := t.Zone()
	if offset == 0 {
		return "UTC"
	}
	if name == "JST" && offset == 9*60*60 {
		return name
	}
	return "UTC" + t.Format("-07:00")
}

func displayTime(at time.Time, format string) string {
	local := at.Local()
	return local.Format(format) + " " + displayZone(local)
}

func sameDisplayDate(a, b time.Time) bool {
	first, second := a.Local(), b.Local()
	return first.Year() == second.Year() && first.YearDay() == second.YearDay()
}
