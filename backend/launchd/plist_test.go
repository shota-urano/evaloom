package launchd

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func TestLaunchAgents(t *testing.T) {
	for _, process := range []string{"evaloomd", "evaloom-runner"} {
		t.Run(process, func(t *testing.T) {
			label := "org.evaloom." + process
			values := readPlist(t, label+".plist")
			for _, key := range []string{"RunAtLoad", "KeepAlive"} {
				if values[key].XMLName.Local != "true" {
					t.Errorf("%s must be true", key)
				}
			}
			if values["Label"].XMLName.Local != "string" || values["Label"].Text != label {
				t.Errorf("Label = %q; want %q", values["Label"].Text, label)
			}
			for key, want := range map[string]string{
				"WorkingDirectory":  "__EVALOOM_ROOT__",
				"StandardOutPath":   "__EVALOOM_ROOT__/data/logs/" + process + ".stdout.log",
				"StandardErrorPath": "__EVALOOM_ROOT__/data/logs/" + process + ".stderr.log",
			} {
				if values[key].XMLName.Local != "string" || values[key].Text != want {
					t.Errorf("%s = %q; want %q", key, values[key].Text, want)
				}
				if rendered := strings.ReplaceAll(values[key].Text, "__EVALOOM_ROOT__", t.TempDir()); !filepath.IsAbs(rendered) {
					t.Errorf("%s does not render to an absolute path: %q", key, rendered)
				}
			}
			arguments := values["ProgramArguments"]
			if arguments.XMLName.Local != "array" {
				t.Fatal("ProgramArguments must be an array")
			}
			var got []string
			for _, argument := range arguments.Children {
				if argument.XMLName.Local != "string" {
					t.Fatal("ProgramArguments must contain only strings")
				}
				got = append(got, argument.Text)
			}
			want := []string{"__EVALOOM_ROOT__/data/bin/" + process}
			if process == "evaloomd" {
				want = append(want, "-db", "__EVALOOM_ROOT__/data/evaloom.db", "-port", "8080", "-web", "__EVALOOM_ROOT__/frontend/dist", "-evaloom-repo", "__EVALOOM_ROOT__")
			} else {
				want = append(want, "-config", "__EVALOOM_ROOT__/data/runner.json")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ProgramArguments = %q; want %q", got, want)
			}
		})
	}
}

func readPlist(t *testing.T, path string) map[string]plistNode {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root plistNode
	if err := xml.Unmarshal(content, &root); err != nil {
		t.Fatal(err)
	}
	if root.XMLName.Local != "plist" || len(root.Children) != 1 || root.Children[0].XMLName.Local != "dict" {
		t.Fatal("want a plist containing one dictionary")
	}
	entries := root.Children[0].Children
	if len(entries)%2 != 0 {
		t.Fatal("plist dictionary has an unmatched key")
	}
	values := make(map[string]plistNode)
	for i := 0; i < len(entries); i += 2 {
		if entries[i].XMLName.Local != "key" {
			t.Fatalf("dictionary entry %d is not a key", i)
		}
		key := entries[i].Text
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate key %q", key)
		}
		values[key] = entries[i+1]
	}
	return values
}

// LaunchServices and Notification Center need an identifiable app bundle that
// runs as an agent without a Dock icon. These settings are part of Q17.
func TestNotificationHelperAppBundle(t *testing.T) {
	values := readPlist(t, filepath.Join("notification-helper", "Info.plist"))
	for key, want := range map[string]string{
		"CFBundleIdentifier":  "org.evaloom.notification-helper",
		"CFBundleName":        "Evaloom Notifications",
		"CFBundleDisplayName": "Evaloom Notifications",
		"CFBundleExecutable":  "evaloom-notification-helper",
		"CFBundlePackageType": "APPL",
	} {
		if got := values[key]; got.XMLName.Local != "string" || got.Text != want {
			t.Errorf("%s = %q; want %q", key, got.Text, want)
		}
	}
	if values["LSUIElement"].XMLName.Local != "true" {
		t.Error("LSUIElement must be true so the helper stays out of the Dock")
	}
	if values["LSBackgroundOnly"].XMLName.Local == "true" {
		t.Error("LSBackgroundOnly prevents an AppKit agent from opening the browser")
	}
}
