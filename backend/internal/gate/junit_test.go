package gate

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseJUnitXMLIDsAndCounts(t *testing.T) {
	file, err := os.Open("testdata/junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	report, err := ParseJUnitXML(file)
	if err != nil {
		t.Fatal(err)
	}
	want := TestReport{
		Total: 6, Passed: 3, Failed: 2, Skipped: 1,
		IDsAvailable: true, TestIDs: []string{"shop.Checkout.accepts & saves [カード]", "shop.Checkout.rejects", "shop.Checkout.timeout", "shop.Users.accepts", "shop.Users.rejects"},
	}
	if !reflect.DeepEqual(report, want) || report.Executed() != 5 {
		t.Fatalf("report = %+v (executed %d), want %+v (executed 5)", report, report.Executed(), want)
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
	report, err := ParseJUnitXML(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := TestReport{Total: 4, Passed: 1, Failed: 2, Skipped: 1,
		IDsAvailable: true, TestIDs: []string{"app.Tests.success", "app.Tests.failure", "app.Tests.error"}}
	if !reflect.DeepEqual(report, want) || report.Executed() != 3 {
		t.Fatalf("report = %+v, want %+v", report, want)
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
		report, err := ParseJUnitXML(strings.NewReader(input))
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
	for _, missing := range []string{`<testcase/>`, `<testcase name=""/>`, `<testcase id="ignored" classname="Class"/>`, `<testcase><skipped/></testcase>`} {
		input := `<testsuite><testcase name="first"/>` + missing + `<testcase name="last"/></testsuite>`
		report, err := ParseJUnitXML(strings.NewReader(input))
		if err != nil || report.IDsAvailable || report.TestIDs != nil || report.Total != 3 {
			t.Fatalf("input=%s report=%+v err=%v", input, report, err)
		}
	}
}

func TestParseJUnitXMLClassnameNameConvention(t *testing.T) {
	input := `<testsuite><testcase id="ignored" classname="FooTest" name="testBar"/><testcase name="plain"/><testcase classname="" name="emptyClass"/><testcase classname="FooTest" name="testBar"/><testcase name="skip"><skipped/></testcase></testsuite>`
	report, err := ParseJUnitXML(strings.NewReader(input))
	want := TestReport{Total: 5, Passed: 4, Skipped: 1, IDsAvailable: true, TestIDs: []string{"FooTest.testBar", "plain", "emptyClass", "FooTest.testBar"}}
	if err != nil || !reflect.DeepEqual(report, want) {
		t.Fatalf("report=%+v err=%v want=%+v", report, err, want)
	}
}

func TestParseJUnitXMLEmptyAndSkippedSuites(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  TestReport
	}{
		{`<testsuite/>`, TestReport{IDsAvailable: true, TestIDs: []string{}}},
		{`<testsuites tests="0"/>`, TestReport{IDsAvailable: true, TestIDs: []string{}}},
		{`<testsuite tests="1" skipped="1"><testcase name="skip"><skipped/></testcase></testsuite>`,
			TestReport{Total: 1, Skipped: 1, IDsAvailable: true, TestIDs: []string{}}},
	} {
		report, err := ParseJUnitXML(strings.NewReader(tc.input))
		if err != nil || !reflect.DeepEqual(report, tc.want) || report.Executed() != 0 {
			t.Fatalf("report = %+v, err = %v, want %+v", report, err, tc.want)
		}
	}
}

func TestParseJUnitXMLPreservesDuplicateIDs(t *testing.T) {
	input := `<testsuite><testcase name="same"/><testcase name="same"/></testsuite>`
	report, err := ParseJUnitXML(strings.NewReader(input))
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
			report, err := ParseJUnitXML(strings.NewReader(tc.input))
			if !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
				t.Fatalf("report = %+v, err = %v; want zero report and ErrJUnitReport", report, err)
			}
		})
	}
}

func TestParseJUnitXMLRejectsDuplicateAttributes(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"root suite", `<testsuite tests="2" tests="1"><testcase id="a"/></testsuite>`},
		{"root suites", `<testsuites tests="2" tests="1"><testsuite><testcase id="a"/></testsuite></testsuites>`},
		{"nested suite", `<testsuites><testsuite tests="2" tests="1"><testcase id="a"/></testsuite></testsuites>`},
		{"testcase", `<testsuite><testcase id="a" id="b"/></testsuite>`},
		{"failure", `<testsuite><testcase id="a"><failure message="first" message="last"/></testcase></testsuite>`},
		{"error", `<testsuite><testcase id="a"><error type="first" type="last"/></testcase></testsuite>`},
		{"skipped", `<testsuite><testcase id="a"><skipped message="first" message="last"/></testcase></testsuite>`},
		{"unknown element", `<testsuite><testcase id="a"/><properties name="first" name="last"/></testsuite>`},
		{"unknown descendant", `<testsuite><testcase id="a"/><properties><property name="first" name="last"/></properties></testsuite>`},
		{"namespace aliases", `<testsuite xmlns:a="urn:report" xmlns:b="urn:report"><testcase id="a" a:label="first" b:label="last"/></testsuite>`},
		{"namespace declaration", `<testsuite xmlns:a="urn:first" xmlns:a="urn:last"/>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := ParseJUnitXML(strings.NewReader(tc.input))
			if !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
				t.Fatalf("report = %+v, err = %v; want zero report and ErrJUnitReport", report, err)
			}
		})
	}
}

func TestParseJUnitXMLAllowsDistinctAttributeNamespaces(t *testing.T) {
	input := `<testsuite xmlns:a="urn:first" xmlns:b="urn:second">
		<testcase name="a" label="plain" a:label="first" b:label="second"/>
	</testsuite>`
	report, err := ParseJUnitXML(strings.NewReader(input))
	want := TestReport{Total: 1, Passed: 1, IDsAvailable: true, TestIDs: []string{"a"}}
	if err != nil || !reflect.DeepEqual(report, want) {
		t.Fatalf("report = %+v, err = %v; want %+v", report, err, want)
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
		report, err := ParseJUnitXML(strings.NewReader(input))
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
		report, err := ParseJUnitXML(reader)
		if !errors.Is(err, wantErr) || !errors.Is(err, ErrJUnitReport) || !reflect.DeepEqual(report, TestReport{}) {
			t.Fatalf("report = %+v, err = %v; want zero report and reader error", report, err)
		}
	}
}
