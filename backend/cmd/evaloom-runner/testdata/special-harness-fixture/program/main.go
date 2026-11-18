// Command fixture is the prepare, check and publish program of the minimal
// special harness used by the contract test (spec 19 Q173). It follows
// harness contract v1 using only the files and the channel Evaloom gives it,
// and never calls an AI.
//
// The task body selects the behaviour with "scenario=<name>":
//   - approval, content_changed: check asks for approval of publish_report.
//   - anything else: check succeeds and publish asks only for "publish".
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const operation = "publish_report"

type request struct {
	Stage string `json:"stage"`
	Task  struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	} `json:"task"`
	Paths struct {
		Workspace string `json:"workspace"`
		Out       string `json:"out"`
		Inputs    string `json:"inputs"`
	} `json:"paths"`
	CheckedContentID *string `json:"checkedContentId"`
	Approvals        []struct {
		Operation string  `json:"operation"`
		ID        *string `json:"id"`
		Status    string  `json:"status"`
	} `json:"approvals"`
}

type result struct {
	ContractVersion  string  `json:"contractVersion"`
	Stage            string  `json:"stage"`
	Status           string  `json:"status"`
	ErrorClass       *string `json:"errorClass"`
	Message          string  `json:"message"`
	ContentID        *string `json:"contentId,omitempty"`
	ApprovalRequests []any   `json:"approvalRequests,omitempty"`
	Outputs          []any   `json:"outputs,omitempty"`
	Usage            any     `json:"usage"`
}

func main() {
	if len(os.Args) != 2 {
		fail(errors.New("usage: fixture prepare|check|publish"))
	}
	stage := os.Args[1]
	raw, err := os.ReadFile(os.Getenv("EVALOOM_REQUEST"))
	fail(err)
	var req request
	fail(json.Unmarshal(raw, &req))
	req.Paths.Workspace, req.Paths.Out, req.Paths.Inputs = hostPath(req.Paths.Workspace), hostPath(req.Paths.Out), hostPath(req.Paths.Inputs)
	if req.Stage != stage {
		fail(fmt.Errorf("request stage %q, command stage %q", req.Stage, stage))
	}
	res := result{ContractVersion: "1", Stage: stage, Status: "succeeded", Message: "fixture " + stage}
	switch stage {
	case "prepare":
		fail(os.WriteFile(filepath.Join(req.Paths.Workspace, "prepared.txt"), []byte("prepared for "+req.Task.ID+"\n"), 0600))
	case "check":
		id := contentID(req)
		res.ContentID = &id
		fail(os.WriteFile(filepath.Join(req.Paths.Out, "report.md"), []byte("checked "+id+"\n"), 0600))
		if s := scenario(req); s == "approval" || s == "content_changed" {
			res.Status = "needs_approval"
			res.ApprovalRequests = []any{map[string]any{"operation": operation, "contentId": id,
				"target": map[string]any{"label": "fixture report"}, "summary": "publish the fixture report"}}
		}
	case "publish":
		res = publish(req, res)
	default:
		fail(fmt.Errorf("unknown stage %q", stage))
	}
	data, err := json.Marshal(res)
	fail(err)
	fail(os.WriteFile(os.Getenv("EVALOOM_RESULT"), data, 0600))
	fmt.Println("fixture finished " + stage)
}

func publish(req request, res result) result {
	report, err := os.ReadFile(filepath.Join(req.Paths.Inputs, "check", "report.md"))
	fail(err)
	if req.CheckedContentID == nil || string(report) != "checked "+*req.CheckedContentID+"\n" {
		fail(errors.New("check output and checkedContentId do not match"))
	}
	// The identifier is recomputed from the real content right before the
	// operation (contract 7.4); Evaloom compares it with the approved one.
	ask := map[string]any{"op": "authorize", "operation": "publish", "approvalId": nil, "contentId": contentID(req)}
	for _, approval := range req.Approvals {
		if approval.Operation == operation {
			ask["operation"], ask["approvalId"] = operation, approval.ID
		}
	}
	conn, err := net.Dial("unix", os.Getenv("EVALOOM_CHANNEL"))
	fail(err)
	defer conn.Close()
	fail(json.NewEncoder(conn).Encode(ask))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	fail(err)
	fail(os.WriteFile(filepath.Join(req.Paths.Out, "authorization.json"), line, 0600))
	var reply struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(line, &reply) != nil || !reply.Allowed {
		class := "publish_failed"
		res.Status, res.ErrorClass, res.Message = "failed", &class, "not allowed: "+reply.Reason
		return res
	}
	// The one external operation of this fixture: write the published copy.
	fail(os.WriteFile(filepath.Join(req.Paths.Out, "published.md"), report, 0600))
	res.Outputs = []any{map[string]any{"kind": "report", "label": "fixture report", "url": "https://example.invalid/fixture/" + req.Task.ID}}
	return res
}

func scenario(req request) string {
	_, after, _ := strings.Cut(req.Task.Body, "scenario=")
	name, _, _ := strings.Cut(after, " ")
	return strings.TrimSpace(name)
}

func contentID(req request) string {
	data, err := os.ReadFile(filepath.Join(req.Paths.Workspace, "prepared.txt"))
	fail(err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hostPath maps a guest path when the test runtime runs this program on the
// host without a mount namespace (EVALOOM_FAKE_MOUNTS: guest target to host
// source). Inside a real container the variable is absent and paths are kept.
func hostPath(path string) string {
	var mounts map[string]string
	if json.Unmarshal([]byte(os.Getenv("EVALOOM_FAKE_MOUNTS")), &mounts) != nil {
		return path
	}
	best := ""
	for target := range mounts {
		if (path == target || strings.HasPrefix(path, target+"/")) && len(target) > len(best) {
			best = target
		}
	}
	if best == "" {
		return path
	}
	return mounts[best] + strings.TrimPrefix(path, best)
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
