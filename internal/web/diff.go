package web

import (
	"html"
	"html/template"
	"strings"
)

// unifiedDiff is a small line diff (LCS) with 3 lines of context, for the
// review step and the revision log. ponytail: O(n*m), fine for config-sized text.
func unifiedDiff(a, b string) string {
	x, y := splitLines(a), splitLines(b)
	if len(x)*len(y) > 4_000_000 { // absurdly large: show a plain replace
		return "@@ whole file replaced @@\n"
	}
	dp := make([][]int32, len(x)+1)
	for i := range dp {
		dp[i] = make([]int32, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []string // " l", "-l", "+l"
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			ops = append(ops, " "+x[i])
			i++
			j++
		case j < len(y) && (i == len(x) || dp[i][j+1] >= dp[i+1][j]):
			ops = append(ops, "+"+y[j])
			j++
		default:
			ops = append(ops, "-"+x[i])
			i++
		}
	}
	const ctx = 3
	keep := make([]bool, len(ops))
	changed := false
	for k, o := range ops {
		if o[0] != ' ' {
			changed = true
			for d := max(0, k-ctx); d <= min(len(ops)-1, k+ctx); d++ {
				keep[d] = true
			}
		}
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	skipped := false
	for k, o := range ops {
		if !keep[k] {
			skipped = true
			continue
		}
		if skipped {
			out.WriteString("@@\n")
			skipped = false
		}
		out.WriteString(o + "\n")
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffHTML colours the +/- lines of a unified diff (everything else is escaped text).
func diffHTML(d string) template.HTML {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		e := html.EscapeString(l)
		switch {
		case strings.HasPrefix(l, "+"):
			e = `<span class="add">` + e + `</span>`
		case strings.HasPrefix(l, "-"):
			e = `<span class="del">` + e + `</span>`
		}
		b.WriteString(e + "\n")
	}
	return template.HTML(b.String())
}
