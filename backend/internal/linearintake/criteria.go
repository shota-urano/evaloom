package linearintake

import (
	"regexp"
	"strings"
)

// atxHeading matches a Markdown ATX heading: up to three spaces of indent,
// one to six '#', then the text (CommonMark 4.2).
var atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)

// closingSequence is an optional run of '#' that closes an ATX heading.
var closingSequence = regexp.MustCompile(`(?:^|[ \t]+)#+$`)

// fenceOpen matches a fenced code block marker (``` or ~~~).
var fenceOpen = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// ExtractAcceptanceCriteria takes the acceptance criteria out of an Issue
// body (spec 15 3.6.1, Q44): the section under the first heading whose text
// is "受け入れ条件" or "Acceptance Criteria" (case-insensitive), up to the
// next heading of the same or a higher level. ok is false when there is no
// such heading or the section is only whitespace ("not written", as in the
// manual creation check of spec 04 3.1). Lines inside fenced code blocks are
// never headings.
func ExtractAcceptanceCriteria(body string) (criteria string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	start, end, level := -1, len(lines), 0
	fence := ""
	for i, line := range lines {
		if m := fenceOpen.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.TrimSpace(line[len(m[0]):]) == "":
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := atxHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		headingLevel := len(m[1])
		if start >= 0 {
			if headingLevel <= level {
				end = i
				break
			}
			continue
		}
		if isCriteriaHeading(closingSequence.ReplaceAllString(m[2], "")) {
			start, level = i+1, headingLevel
		}
	}
	if start < 0 {
		return "", false
	}
	criteria = strings.TrimSpace(strings.Join(lines[start:end], "\n"))
	return criteria, criteria != ""
}

func isCriteriaHeading(text string) bool {
	text = strings.Join(strings.Fields(text), " ")
	return text == "受け入れ条件" || strings.EqualFold(text, "Acceptance Criteria")
}
