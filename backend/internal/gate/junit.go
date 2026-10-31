// Package gate implements the Trusted Runner's verification gates (spec 10).
package gate

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrJUnitReport means the report cannot supply trustworthy JUnit results.
// Gate 2's caller owns error classification and persistence.
var ErrJUnitReport = errors.New("gate: invalid JUnit XML report")

// JUnitTestID preserves the report's attributes without choosing an ID format.
type JUnitTestID struct {
	ID        string `xml:"id,attr"`
	ClassName string `xml:"classname,attr"`
	Name      string `xml:"name,attr"`
}

// JUnitIDResolver supplies the runner-specific ID convention, which spec 10
// does not define. Baseline and verification must use the same resolver.
// Returning false means an ID cannot be obtained for this executed testcase.
type JUnitIDResolver interface {
	ResolveJUnitID(JUnitTestID) (string, bool)
}

// TestReport contains counts for test_runs (spec 02) and executed test IDs.
// Total includes skipped cases; failed includes both failures and errors.
// IDsAvailable is false if the report lacks case details, resolver is nil, or
// any executed case's ID is unavailable. TestIDs is then nil, never partial.
type TestReport struct {
	Total, Passed, Failed, Skipped int
	TestIDs                        []string
	IDsAvailable                   bool
}

// Executed excludes skipped cases, which do not establish test execution.
func (report TestReport) Executed() int { return report.Total - report.Skipped }

// ParseJUnitXML reads one complete testsuite or testsuites document. Counts
// come from testcase outcomes, or suite counters when case details are absent.
// Nested suite counters are checked, not added to their children's counts.
// Malformed XML, contradictory counters, and reader errors return no results.
// The reader's owner handles report paths, sandbox access and file lifetimes.
func ParseJUnitXML(reader io.Reader, resolver JUnitIDResolver) (TestReport, error) {
	decoder := xml.NewDecoder(reader)
	var root *junitSuite
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return TestReport{}, fmt.Errorf("%w: %w", ErrJUnitReport, err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if root != nil || (token.Name.Local != "testsuite" && token.Name.Local != "testsuites") {
				return TestReport{}, ErrJUnitReport
			}
			root = &junitSuite{}
			if err := decoder.DecodeElement(root, &token); err != nil {
				return TestReport{}, fmt.Errorf("%w: %w", ErrJUnitReport, err)
			}
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return TestReport{}, ErrJUnitReport
			}
		case xml.EndElement:
			return TestReport{}, ErrJUnitReport
		}
	}
	if root == nil {
		return TestReport{}, ErrJUnitReport
	}
	counts, cases, complete, err := root.results()
	if err != nil {
		return TestReport{}, fmt.Errorf("%w: %w", ErrJUnitReport, err)
	}
	report := TestReport{
		Total: counts.total, Failed: counts.failed(), Skipped: counts.skipped,
		Passed: counts.total - counts.failed() - counts.skipped,
	}
	if !complete || resolver == nil {
		return report, nil
	}
	ids := make([]string, 0, report.Executed())
	for _, testcase := range cases {
		id, ok := resolver.ResolveJUnitID(testcase)
		if !ok || id == "" {
			return report, nil
		}
		ids = append(ids, id)
	}
	report.TestIDs, report.IDsAvailable = ids, true
	return report, nil
}

type junitSuite struct {
	Tests    *int         `xml:"tests,attr"`
	Failures *int         `xml:"failures,attr"`
	Errors   *int         `xml:"errors,attr"`
	Skipped  *int         `xml:"skipped,attr"`
	Cases    []junitCase  `xml:"testcase"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	JUnitTestID
	Skipped *struct{} `xml:"skipped"`
	Failure *struct{} `xml:"failure"`
	Error   *struct{} `xml:"error"`
}

type junitCounts struct {
	total, failures, errors, skipped int
}

func (counts junitCounts) failed() int { return counts.failures + counts.errors }

func (suite junitSuite) results() (junitCounts, []JUnitTestID, bool, error) {
	var counts junitCounts
	var cases []JUnitTestID
	complete := true
	for _, testcase := range suite.Cases {
		counts.total++
		switch {
		case testcase.Skipped != nil:
			if testcase.Failure != nil || testcase.Error != nil {
				return counts, nil, false, errors.New("conflicting testcase outcomes")
			}
			counts.skipped++
		case testcase.Failure != nil && testcase.Error != nil:
			return counts, nil, false, errors.New("conflicting testcase outcomes")
		case testcase.Failure != nil:
			counts.failures++
		case testcase.Error != nil:
			counts.errors++
		}
		if testcase.Skipped == nil {
			cases = append(cases, testcase.JUnitTestID)
		}
	}
	for _, child := range suite.Suites {
		childCounts, childCases, childComplete, err := child.results()
		if err != nil {
			return counts, nil, false, err
		}
		// Every component is bounded by total, so checking total also bounds
		// the component sums. Attribute overflow is rejected by encoding/xml.
		if childCounts.total > int(^uint(0)>>1)-counts.total {
			return counts, nil, false, errors.New("test count overflow")
		}
		counts.total += childCounts.total
		counts.failures += childCounts.failures
		counts.errors += childCounts.errors
		counts.skipped += childCounts.skipped
		cases = append(cases, childCases...)
		complete = complete && childComplete
	}
	// With no testcase or child suite, retain summary counts for the spec's
	// count-only fallback. An empty suite without counters has zero cases.
	if len(suite.Cases) == 0 && len(suite.Suites) == 0 {
		if suite.Tests != nil {
			counts.total = *suite.Tests
		}
		if suite.Failures != nil {
			counts.failures = *suite.Failures
		}
		if suite.Errors != nil {
			counts.errors = *suite.Errors
		}
		if suite.Skipped != nil {
			counts.skipped = *suite.Skipped
		}
		complete = counts.total == 0
	}
	if counts.total < 0 || counts.failures < 0 || counts.errors < 0 || counts.skipped < 0 ||
		counts.failures > counts.total || counts.errors > counts.total-counts.failures ||
		counts.skipped > counts.total-counts.failures-counts.errors {
		return counts, nil, false, errors.New("invalid suite counts")
	}
	for _, counter := range []struct {
		declared *int
		actual   int
	}{
		{suite.Tests, counts.total}, {suite.Failures, counts.failures},
		{suite.Errors, counts.errors}, {suite.Skipped, counts.skipped},
	} {
		if counter.declared != nil && *counter.declared != counter.actual {
			return counts, nil, false, errors.New("suite counters disagree with results")
		}
	}
	return counts, cases, complete, nil
}
