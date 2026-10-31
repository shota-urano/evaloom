package gate

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

type junitResolverFunc func(JUnitTestID) (string, bool)

func (resolve junitResolverFunc) ResolveJUnitID(id JUnitTestID) (string, bool) {
	return resolve(id)
}

// Test-only convention; the product's ID normalization is not specified.
var attributeID = junitResolverFunc(func(id JUnitTestID) (string, bool) {
	return id.ID, id.ID != ""
})

func TestParseJUnitXMLIDsAndCounts(t *testing.T) {
	file, err := os.Open("testdata/junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var attributes []JUnitTestID
	report, err := ParseJUnitXML(file, junitResolverFunc(func(id JUnitTestID) (string, bool) {
		attributes = append(attributes, id)
		return attributeID.ResolveJUnitID(id)
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := TestReport{
		Total: 6, Passed: 3, Failed: 2, Skipped: 1,
		IDsAvailable: true, TestIDs: []string{"case-1", "case-2", "case-3", "case-5", "case-6"},
	}
	if !reflect.DeepEqual(report, want) || report.Executed() != 5 {
		t.Fatalf("report = %+v (executed %d), want %+v (executed 5)", report, report.Executed(), want)
	}
	if len(attributes) != 5 || attributes[0] != (JUnitTestID{
		ID: "case-1", ClassName: "shop.Checkout", Name: "accepts & saves [カード]",
	}) {
		t.Fatalf("resolver attributes = %+v", attributes)
	}
}

func TestParseJUnitXMLSingleSuiteWithoutCounters(t *testing.T) {
	// This shape matches internal/securitytest/assets/test.py: classname/name,
	// optional summary attributes, and failure/error/skipped child elements.
	input := `<testsuite name="fixture">
		<testcase classname="app.Tests" name="success"/>
		<testcase classname="app.Tests" name="failure"><failure/></testcase>
		<testcase classname="app.Tests" name="error"><error/></testcase>
		<testcase classname="app.Tests" name="skip"><skipped message="later"/></testcase>
	</testsuite>`
	var names []string
	report, err := ParseJUnitXML(strings.NewReader(input), junitResolverFunc(func(id JUnitTestID) (string, bool) {
		if id.ClassName != "app.Tests" || id.ID != "" {
			t.Fatalf("attributes = %+v", id)
		}
		names = append(names, id.Name)
		return id.Name, true
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := TestReport{Total: 4, Passed: 1, Failed: 2, Skipped: 1,
		IDsAvailable: true, TestIDs: []string{"success", "failure", "error"}}
	if !reflect.DeepEqual(report, want) || !reflect.DeepEqual(names, want.TestIDs) || report.Executed() != 3 {
		t.Fatalf("report = %+v, resolver names = %v, want %+v", report, names, want)
	}
}

func TestParseJUnitXMLCountOnly(t *testing.T) {
	for _, input := range []string{
		`<testsuite tests="7" failures="1" errors="2" skipped="1"/>`,
		`<testsuites tests="7" failures="1" errors="2" skipped="1">
			<testsuite tests="3" failures="1" skipped="1"/>
			<testsuite tests="4" errors="2"/>
		</testsuites>`,
		`<testsuites tests="7" failures="1" errors="2" skipped="1">
			<testsuite tests="6" failures="1" errors="2" skipped="1"/>
			<testsuite><testcase id="known"/></testsuite>
		</testsuites>`,
	} {
		report, err := ParseJUnitXML(strings.NewReader(input), attributeID)
		if err != nil {
			t.Fatal(err)
		}
		want := TestReport{Total: 7, Passed: 3, Failed: 3, Skipped: 1}
		if !reflect.DeepEqual(report, want) || report.Executed() != 6 {
			t.Fatalf("report = %+v, want %+v", report, want)
		}
	}
}

func TestParseJUnitXMLUnavailableIDsDoNotReturnPartialList(t *testing.T) {
	input := `<testsuite><testcase id="first"/><testcase/><testcase id="last"/></testsuite>`
	for _, resolver := range []JUnitIDResolver{
		nil,
		attributeID,
		junitResolverFunc(func(id JUnitTestID) (string, bool) { return id.ID, true }),
	} {
		report, err := ParseJUnitXML(strings.NewReader(input), resolver)
		if err != nil {
			t.Fatal(err)
		}
		if want := (TestReport{Total: 3, Passed: 3}); !reflect.DeepEqual(report, want) {
			t.Fatalf("report = %+v, want %+v", report, want)
		}
	}
}

func TestParseJUnitXMLEmptyAndSkippedSuites(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  TestReport
	}{
		{`<testsuite/>`, TestReport{IDsAvailable: true, TestIDs: []string{}}},
		{`<testsuites tests="0"/>`, TestReport{IDsAvailable: true, TestIDs: []string{}}},
		{`<testsuite tests="1" skipped="1"><testcase><skipped/></testcase></testsuite>`,
			TestReport{Total: 1, Skipped: 1, IDsAvailable: true, TestIDs: []string{}}},
	} {
		report, err := ParseJUnitXML(strings.NewReader(tc.input), attributeID)
		if err != nil || !reflect.DeepEqual(report, tc.want) || report.Executed() != 0 {
			t.Fatalf("report = %+v, err = %v, want %+v", report, err, tc.want)
		}
	}
}

func TestParseJUnitXMLPreservesDuplicateIDs(t *testing.T) {
	input := `<testsuite><testcase id="same"/><testcase id="same"/></testsuite>`
	report, err := ParseJUnitXML(strings.NewReader(input), attributeID)
	if err != nil || report.Total != 2 || !reflect.DeepEqual(report.TestIDs, []string{"same", "same"}) {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestParseJUnitXMLMalformedXML(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"truncated", `<testsuite><testcase id="first"/>`},
		{"mismatched tags", `<testsuite><testcase></testsuite>`},
		{"invalid entity", `<testsuite><testcase name="&unknown;"/></testsuite>`},
		{"invalid UTF-8", "<testsuite>\xff</testsuite>"},
		{"trailing malformed XML", `<testsuite><testcase id="first"/></testsuite><`},
		{"multiple roots", `<testsuite/><testsuite/>`},
		{"leading text", `not xml<testsuite/>`},
		{"trailing text", `<testsuite/>not xml`},
		{"wrong root", `<report><testcase/></report>`},
		{"noninteger counter", `<testsuite tests="two"/>`},
		{"counter overflow", `<testsuite tests="999999999999999999999999999999999"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := ParseJUnitXML(strings.NewReader(tc.input), attributeID)
			if !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
				t.Fatalf("report = %+v, err = %v; want zero report and ErrJUnitReport", report, err)
			}
		})
	}
}

func TestParseJUnitXMLRejectsContradictoryResults(t *testing.T) {
	for _, input := range []string{
		`<testsuite tests="-1"/>`,
		`<testsuite tests="1" skipped="-1"/>`,
		`<testsuite tests="1" failures="1" errors="1"/>`,
		`<testsuite tests="1" failures="1" skipped="1"/>`,
		`<testsuite tests="2"><testcase/></testsuite>`,
		`<testsuite tests="1" skipped="1"><testcase/></testsuite>`,
		`<testsuite failures="1"><testcase><error/></testcase></testsuite>`,
		`<testsuites tests="3"><testsuite tests="2"/></testsuites>`,
		`<testsuite><testcase><failure/><skipped/></testcase></testsuite>`,
		`<testsuite><testcase><error/><skipped/></testcase></testsuite>`,
		`<testsuite><testcase><failure/><error/></testcase></testsuite>`,
	} {
		report, err := ParseJUnitXML(strings.NewReader(input), attributeID)
		if !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
			t.Fatalf("input %s: report = %+v, err = %v; want zero report and ErrJUnitReport", input, report, err)
		}
	}
}

type junitErrorReader struct{ err error }

func (reader junitErrorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestParseJUnitXMLReaderErrors(t *testing.T) {
	wantErr := errors.New("report read failed")
	for _, prefix := range []string{"", `<testsuite><testcase/>`, `<testsuite/>`} {
		reader := io.MultiReader(strings.NewReader(prefix), junitErrorReader{wantErr})
		report, err := ParseJUnitXML(reader, attributeID)
		if !errors.Is(err, wantErr) || !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
			t.Fatalf("report = %+v, err = %v; want zero report and reader error", report, err)
		}
	}
}
