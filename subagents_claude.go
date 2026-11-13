package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const childParentBytes = 2 << 20
const childTailBytes = 32 << 10
const childMetaBytes = 16 << 10

var childNativeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Deliberately separate from the human/assistant history decoder. Free-form
// prompts/results/arguments are never projected into the summary wire model.
type childRecord struct {
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	SessionID string    `json:"sessionId"`
	AgentID   string    `json:"agentId"`
	Sidechain bool      `json:"isSidechain"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"sourceToolAssistantUUID"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Result     childToolResult `json:"toolUseResult"`
	Attachment struct {
		Type      string    `json:"type"`
		Mode      string    `json:"commandMode"`
		Prompt    string    `json:"prompt"`
		Delivery  string    `json:"delivery_id"`
		Timestamp time.Time `json:"timestamp"`
		Origin    struct {
			Kind     string `json:"kind"`
			Producer string `json:"producer"`
		} `json:"origin"`
	} `json:"attachment"`
}
type childToolResult struct {
	AgentID      string `json:"agentId"`
	Status       string `json:"status"`
	IsAsync      bool   `json:"isAsync"`
	ToolEndsTurn bool   `json:"toolEndsTurn"`
}

func (r *childToolResult) UnmarshalJSON(b []byte) error {
	// Ordinary tool results can be strings/arrays. They are not a malformed
	// child source and must not erase the surrounding record's identity.
	type plain childToolResult
	var p plain
	if len(b) > 0 && b[0] == '{' {
		_ = json.Unmarshal(b, &p)
	}
	*r = childToolResult(p)
	return nil
}

type childBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}
type childMeta struct {
	AgentType  string `json:"agentType"`
	ToolUseID  string `json:"toolUseId"`
	SpawnDepth int    `json:"spawnDepth"`
	Finished   bool   `json:"finished"`
}
type childLaunch struct {
	row, call string
	block     int
	at        time.Time
}
type childResult struct {
	child, call, source, status string
	at                          time.Time
	async                       bool
}
type childTerminal struct {
	child, call, status string
	at                  time.Time
}

// readChildFile uses an anchored os.Root and rejects file symlinks, including
// links to other children. Directory roots below the verified parent are also
// opened without symlinks. No outputFile value is ever opened.
func readChildFile(root *os.Root, name string, budget int, tail bool) ([]byte, os.FileInfo, bool, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, false, os.ErrPermission
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(st, actual) {
		return nil, nil, false, os.ErrPermission
	}
	truncated := st.Size() > int64(budget)
	if truncated && tail {
		if _, err = f.Seek(st.Size()-int64(budget), io.SeekStart); err != nil {
			return nil, st, true, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(budget)))
	if truncated && tail {
		if n := bytes.IndexByte(b, '\n'); n >= 0 {
			b = b[n+1:]
		} else {
			b = nil
		}
	}
	after, statErr := f.Stat()
	if statErr != nil || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
		return nil, st, true, os.ErrInvalid
	}
	return b, st, truncated, err
}
func childRecords(b []byte) ([]childRecord, bool) {
	records := []childRecord{}
	complete := true
	if len(b) > 0 && b[len(b)-1] != '\n' {
		complete = false
		if n := bytes.LastIndexByte(b, '\n'); n >= 0 {
			b = b[:n+1]
		} else {
			b = nil
		}
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r childRecord
		if json.Unmarshal(line, &r) != nil {
			complete = false
			continue
		}
		records = append(records, r)
	}
	return records, complete
}
func childBlocks(r childRecord) []childBlock {
	var b []childBlock
	_ = json.Unmarshal(r.Message.Content, &b)
	return b
}
func openChildDir(root *os.Root, name string) (*os.Root, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrPermission
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		child.Close()
		return nil, os.ErrPermission
	}
	return child, nil
}
func childSafeName(name string) string {
	// Tool names only: never arguments, descriptions, paths, output or reasoning.
	if !childNativeID.MatchString(name) {
		return "Tool"
	}
	return name
}
func readClaudeChildren(ctx context.Context, se sessionInfo, path, native, scope string, now time.Time) subagentList {
	list := emptyChildren(se, now)
	if !childNativeID.MatchString(native) || filepath.Base(path) != native+".jsonl" {
		return list
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return list
	}
	defer root.Close()
	parent, st, truncated, err := readChildFile(root, native+".jsonl", childParentBytes, true)
	if err != nil {
		return list
	}
	// A source replacement/rewrite, including same-length edits, invalidates
	// cursors. Appends also change the revision/segment in S1; S4 can preserve
	// append epochs once incremental ingestion is implemented.
	list.Segment = childToken(scope, st.ModTime().UTC().String(), string(parent))
	list.Availability = "available"
	list.Complete = !truncated
	list.Truncated = truncated
	records, complete := childRecords(parent)
	list.Complete = list.Complete && complete
	launches := map[string]childLaunch{}
	results := []childResult{}
	terminals := []childTerminal{}
	deliveries := map[string]bool{}
	for _, r := range records {
		if ctx.Err() != nil {
			list.Availability = "unavailable"
			list.Complete = false
			return list
		}
		if r.SessionID != "" && r.SessionID != native {
			list.Complete = false
			continue
		}
		if r.Sidechain {
			continue
		}
		blocks := childBlocks(r)
		for i, b := range blocks {
			if r.Type == "assistant" && b.Type == "tool_use" && b.Name == "Agent" && childNativeID.MatchString(b.ID) && childNativeID.MatchString(r.UUID) {
				launches[b.ID] = childLaunch{r.UUID, b.ID, i, r.Timestamp}
			}
		}
		if r.Type == "user" && childNativeID.MatchString(r.Result.AgentID) {
			// Ambiguous multi-result wrappers cannot safely assign their top-level result.
			call := ""
			n := 0
			for _, b := range blocks {
				if b.Type == "tool_result" {
					call = b.ToolUseID
					n++
				}
			}
			if n == 1 {
				results = append(results, childResult{r.Result.AgentID, call, r.Source, r.Result.Status, r.Timestamp, r.Result.IsAsync})
			}
		}
		a := r.Attachment
		if r.Type == "attachment" && a.Type == "queued_command" && a.Mode == "task-notification" && a.Origin.Kind == "task-notification" && a.Origin.Producer == "session-task" {
			// Ignore queue/prose copies. A delivery ID is deduplicated independently
			// of child identity, so verified continuation remains a separate edge.
			if a.Delivery != "" && deliveries[a.Delivery] {
				continue
			}
			if a.Delivery != "" {
				deliveries[a.Delivery] = true
			}
			var n struct {
				XMLName xml.Name `xml:"task-notification"`
				ID      string   `xml:"task-id"`
				Call    string   `xml:"tool-use-id"`
				Status  string   `xml:"status"`
			}
			if xml.Unmarshal([]byte(a.Prompt), &n) == nil && childNativeID.MatchString(n.ID) {
				status := ""
				switch n.Status {
				case "completed":
					status = "succeeded"
				case "failed":
					status = "failed"
				case "killed", "cancelled", "canceled":
					status = "canceled"
				}
				at := a.Timestamp
				if at.IsZero() {
					at = r.Timestamp
				}
				if status != "" {
					terminals = append(terminals, childTerminal{n.ID, n.Call, status, at})
				}
			}
		}
	}
	byID := map[string]*childRun{}
	resultChildren := map[string]string{}
	for _, r := range results {
		if l, ok := launches[r.call]; ok && (r.source == "" || r.source == l.row) {
			resultChildren[r.call] = r.child
		}
	}
	latest := map[string]time.Time{}
	terminalAt := map[string]time.Time{}
	get := func(id string) *childRun {
		if child := byID[id]; child != nil {
			return child
		}
		if len(byID) >= childMaxRuns {
			list.Truncated = true
			list.Complete = false
			return nil
		}
		child := &childRun{ID: childToken(scope, native, id), Kind: "agent", Label: "Agent", Status: "unknown", StateSource: "identity", Freshness: "stale", Availability: "unavailable", Invocations: []childInvocation{}}
		byID[id] = child
		return child
	}
	link := func(id string, child *childRun, l childLaunch) {
		child.Linked = true
		for _, edge := range child.Invocations {
			if edge.ParentToolCallID == l.call {
				return
			}
		}
		if len(child.Invocations) >= 32 {
			list.Truncated = true
			list.Complete = false
			return
		}
		child.Invocations = append(child.Invocations, childInvocation{ID: childToken(scope, native, id, l.row, l.call), ParentRowID: l.row, ParentToolCallID: l.call, ParentBlock: l.block, StartedAt: l.at})
		if l.at.After(latest[id]) {
			latest[id] = l.at
		}
	}
	for _, r := range results {
		l, ok := launches[r.call]
		if !ok || (r.source != "" && r.source != l.row) {
			continue
		}
		child := get(r.child)
		if child == nil {
			continue
		}
		link(r.child, child, l)
		if r.at.After(latest[r.child]) {
			latest[r.child] = r.at
		}
		if r.async && r.status == "async_launched" && !r.at.IsZero() {
			child.Status = "running"
			child.StateSource = "async-launch"
			child.EvidenceAt = r.at
		}
	}
	// Only discover this parent's direct directory; never recursively follow
	// child directories or scan other projects for candidate children.
	sessionDir, dirErr := openChildDir(root, native)
	var childrenDir *os.Root
	if dirErr == nil {
		defer sessionDir.Close()
		childrenDir, dirErr = openChildDir(sessionDir, "subagents")
	}
	if dirErr == nil {
		defer childrenDir.Close()
		dir, err := childrenDir.Open(".")
		if err != nil {
			list.Complete = false
		} else {
			entries, readErr := dir.ReadDir(childMaxRuns*2 + 1)
			dir.Close()
			if readErr != nil && readErr != io.EOF {
				list.Complete = false
			}
			if len(entries) > childMaxRuns*2 {
				list.Complete = false
				list.Truncated = true
				entries = entries[:childMaxRuns*2]
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			ids := map[string]bool{}
			for _, entry := range entries {
				name := entry.Name()
				if !strings.HasPrefix(name, "agent-") {
					continue
				}
				id := strings.TrimPrefix(name, "agent-")
				id = strings.TrimSuffix(strings.TrimSuffix(id, ".meta.json"), ".jsonl")
				if !childNativeID.MatchString(id) || ids[id] {
					continue
				}
				ids[id] = true
				if ctx.Err() != nil {
					list.Complete = false
					break
				}
				stem := "agent-" + id
				head, _, _, _ := readChildFile(childrenDir, stem+".jsonl", childTailBytes, false)
				headRows, _ := childRecords(head)
				// The first child record must prove native parent + child identity. Its
				// parentUuid is an internal message-chain link and may be null.
				identity := len(headRows) > 0 && headRows[0].AgentID == id && headRows[0].SessionID == native && headRows[0].Sidechain
				metaBytes, metaStat, metaTruncated, metaErr := readChildFile(childrenDir, stem+".meta.json", childMetaBytes, false)
				var meta childMeta
				metaOK := metaErr == nil && !metaTruncated && json.Unmarshal(metaBytes, &meta) == nil
				if metaOK && meta.SpawnDepth > 1 {
					delete(byID, id)
					continue
				}
				if !identity {
					if child := byID[id]; child != nil {
						child.Availability = "unavailable"
					}
					list.Complete = false
					continue
				}
				child := get(id)
				if child == nil {
					continue
				}
				if metaOK {
					if l, ok := launches[meta.ToolUseID]; ok && (resultChildren[meta.ToolUseID] == "" || resultChildren[meta.ToolUseID] == id) {
						link(id, child, l)
					}
					child.Label = childSafeName(meta.AgentType)
				}
				tail, _, tailTruncated, tailErr := readChildFile(childrenDir, stem+".jsonl", childTailBytes, true)
				if tailErr != nil {
					list.Complete = false
					continue
				}
				child.Availability = "available"
				rows, rowsOK := childRecords(tail)
				if !rowsOK {
					list.Complete = false
				}
				if tailTruncated {
					list.Truncated = true
					list.Complete = false
				}
				handbacks := map[string]bool{}
				for _, r := range rows {
					if r.AgentID != id || r.SessionID != native || !r.Sidechain {
						continue
					}
					for _, b := range childBlocks(r) {
						if r.Type == "assistant" && b.Type == "tool_use" {
							if b.Name == "SubagentHandback" {
								handbacks[b.ID] = true
							}
							if child.LastAction == nil || r.Timestamp.After(child.LastAction.At) {
								child.LastAction = &childAction{Kind: "tool", Name: childSafeName(b.Name), At: r.Timestamp}
							}
						}
						if r.Type == "user" && b.Type == "tool_result" && handbacks[b.ToolUseID] && r.Result.ToolEndsTurn && !b.IsError && !r.Timestamp.Before(latest[id]) {
							child.Status = "finished"
							child.StateSource = "handback"
							child.EvidenceAt = r.Timestamp
							terminalAt[id] = r.Timestamp
						}
					}
					if r.Type == "assistant" && r.Timestamp.After(child.EvidenceAt) && terminalAt[id].IsZero() && !r.Timestamp.Before(latest[id]) {
						child.Status = "running"
						child.StateSource = "child-activity"
						child.EvidenceAt = r.Timestamp
					}
				}
				if metaOK && meta.Finished && !metaStat.ModTime().Before(latest[id]) {
					child.Status = "finished"
					child.StateSource = "metadata-finished"
					child.EvidenceAt = metaStat.ModTime().UTC()
					terminalAt[id] = child.EvidenceAt
				}
			}
		}
	} else if !os.IsNotExist(dirErr) {
		list.Complete = false
		list.Availability = "unavailable"
	}
	for _, n := range terminals {
		child := byID[n.child]
		if child == nil || !child.Linked || n.at.Before(latest[n.child]) || n.at.IsZero() {
			continue
		}
		matched := n.call == ""
		for i := range child.Invocations {
			if n.call == child.Invocations[i].ParentToolCallID {
				matched = true
				child.Invocations[i].Outcome = n.status
			}
		}
		if matched && (child.StateSource == "metadata-finished" || !n.at.Before(terminalAt[n.child])) {
			child.Status = n.status
			child.StateSource = "task-notification"
			child.EvidenceAt = n.at
			terminalAt[n.child] = n.at
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		child := byID[id]
		if !child.Linked {
			child.Status = "unknown"
			child.StateSource = "unlinked"
		}
		list.Children = append(list.Children, *child)
	}
	finishChildList(&list, now)
	return list
}

// At the coarse six-second cadence, stat bounded source entries first. Unchanged
// roots reuse decoded summaries, so extra phones and quiet children add no JSONL
// readers. Detailed incremental tails and invalidations belong to S4.
func childSourceFiles(path, native string) map[string]os.FileInfo {
	if !childNativeID.MatchString(native) {
		return nil
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer root.Close()
	files := map[string]os.FileInfo{}
	st, err := root.Lstat(native + ".jsonl")
	if err != nil {
		return nil
	}
	files["parent"] = st
	dir, err := openChildDir(root, native)
	if os.IsNotExist(err) {
		return files
	}
	if err != nil {
		return nil
	}
	defer dir.Close()
	st, err = dir.Stat(".")
	if err != nil {
		return nil
	}
	files["session-dir"] = st
	children, err := openChildDir(dir, "subagents")
	if os.IsNotExist(err) {
		return files
	}
	if err != nil {
		return nil
	}
	defer children.Close()
	st, err = children.Stat(".")
	if err != nil {
		return nil
	}
	files["children-dir"] = st
	f, err := children.Open(".")
	if err != nil {
		return nil
	}
	defer f.Close()
	entries, err := f.ReadDir(childMaxRuns*2 + 1)
	if err != nil && err != io.EOF {
		return nil
	}
	// An oversized directory cannot be proven unchanged with a bounded sample.
	if len(entries) > childMaxRuns*2 {
		return nil
	}
	for _, e := range entries {
		st, err := children.Lstat(e.Name())
		if err != nil {
			return nil
		}
		files[e.Name()] = st
	}
	return files
}
func sameChildFiles(a, b map[string]os.FileInfo) bool {
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for name, x := range a {
		y := b[name]
		if y == nil || !os.SameFile(x, y) || x.Size() != y.Size() || x.Mode() != y.Mode() || !x.ModTime().Equal(y.ModTime()) {
			return false
		}
	}
	return true
}
