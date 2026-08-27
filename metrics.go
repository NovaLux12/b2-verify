package main

import (
	"strconv"
	"strings"
)

// buildMetrics renders the Prometheus text exposition format for the current
// results. The format is hand-rolled deliberately: the tool has zero runtime
// dependencies beyond the Go standard library.
func buildMetrics(results []legResult, lastRun float64, cfg Config) string {
	var sb strings.Builder

	writeFamily := func(name, help, typ string) {
		sb.WriteString("# HELP ")
		sb.WriteString(name)
		sb.WriteString(" ")
		sb.WriteString(help)
		sb.WriteString("\n# TYPE ")
		sb.WriteString(name)
		sb.WriteString(" ")
		sb.WriteString(typ)
		sb.WriteString("\n")
	}

	writeFamily("b2_verify_leg_age_seconds", "Age in seconds of the newest object on each monitored leg.", "gauge")
	for _, r := range results {
		writeSeries(&sb, "b2_verify_leg_age_seconds", r.leg.Name, strconv.FormatFloat(r.age, 'f', 3, 64), "")
	}

	writeFamily("b2_verify_leg_fresh", "Whether the leg is fresh: 1 = fresh, 0 = stale or error.", "gauge")
	for _, r := range results {
		fresh := 0
		if r.state == stateFresh {
			fresh = 1
		}
		writeSeries(&sb, "b2_verify_leg_fresh", r.leg.Name, strconv.Itoa(fresh), "")
	}

	writeFamily("b2_verify_last_run_timestamp_seconds", "Unix time in seconds of the last completed check run.", "gauge")
	writeSeries(&sb, "b2_verify_last_run_timestamp_seconds", "", strconv.FormatFloat(lastRun, 'f', 0, 64), "")

	writeFamily("b2_verify_scrape", "Information about this scrape: version and number of configured legs.", "gauge")
	writeSeries(&sb, "b2_verify_scrape", "", "1", `legs=`+strconv.Quote(strconv.Itoa(len(cfg.Legs)))+`,version=`+strconv.Quote(version))

	return sb.String()
}

// writeSeries emits one metric line: name{label=...} value.
func writeSeries(sb *strings.Builder, name, legName, value, extraLabels string) {
	sb.WriteString(name)
	switch {
	case legName != "" && extraLabels != "":
		sb.WriteString(`{leg="`)
		sb.WriteString(escapeLabelValue(legName))
		sb.WriteString(`",`)
		sb.WriteString(extraLabels)
		sb.WriteString("}")
	case legName != "":
		sb.WriteString(`{leg="`)
		sb.WriteString(escapeLabelValue(legName))
		sb.WriteString(`"}`)
	case extraLabels != "":
		sb.WriteString("{")
		sb.WriteString(extraLabels)
		sb.WriteString("}")
	}
	sb.WriteString(" ")
	sb.WriteString(value)
	sb.WriteString("\n")
}

// escapeLabelValue escapes a label value per the Prometheus text format:
// backslash, double quote and newline only; all other UTF-8 passes through.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var sb strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
