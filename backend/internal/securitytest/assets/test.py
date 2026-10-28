from pathlib import Path
import unittest
import xml.etree.ElementTree as ET
from probe import run

report = Path("junit.xml")
report.unlink(missing_ok=True)
run("test-plan.json", "test-results.json")
suite_xml = ET.Element("testsuite", name="fixture")


class JUnitResult(unittest.TextTestResult):
    def startTest(self, test):
        super().startTest(test)
        self.case = ET.SubElement(suite_xml, "testcase", name=test.id().rsplit(".", 1)[1],
                                  classname=test.id().rsplit(".", 1)[0])

    def addFailure(self, test, err):
        super().addFailure(test, err)
        ET.SubElement(self.case, "failure").text = self._exc_info_to_string(err, test)

    def addError(self, test, err):
        super().addError(test, err)
        ET.SubElement(self.case, "error").text = self._exc_info_to_string(err, test)

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        ET.SubElement(self.case, "skipped", message=reason)


suite = unittest.defaultTestLoader.discover(".", pattern="test_app.py")
result = unittest.TextTestRunner(resultclass=JUnitResult).run(suite)
suite_xml.set("tests", str(result.testsRun))
suite_xml.set("failures", str(len(result.failures)))
suite_xml.set("errors", str(len(result.errors)))
suite_xml.set("skipped", str(len(result.skipped)))
ET.ElementTree(suite_xml).write(report, encoding="utf-8", xml_declaration=True)
raise SystemExit(0 if result.wasSuccessful() and result.testsRun > 0 else 1)
