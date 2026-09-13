package main

import (
	"context"
	"errors"
)

// sessionCapabilities lets clients hide controls that cannot be made safe for
// the active harness. Structured=false means the terminal remains the only
// trustworthy surface.
type sessionCapabilities struct {
	Structured    bool              `json:"structured"`
	History       bool              `json:"history"`
	Replies       bool              `json:"replies"`
	Activity      bool              `json:"activity"`
	Ask           bool              `json:"ask"`
	Slash         bool              `json:"slash"`
	Queue         bool              `json:"queue"`
	Steer         bool              `json:"steer"`
	Interrupt     bool              `json:"interrupt"`
	Attention     bool              `json:"attention"`
	Approvals     bool              `json:"approvals"`
	Questions     bool              `json:"questions"`
	SlashCommands []slashCapability `json:"slashCommands,omitempty"`
}

type slashCapability struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

var claudeSlashCapabilities = []slashCapability{
	{Command: "/compact", Description: "condense the conversation"},
	{Command: "/context", Description: "show context usage"},
	{Command: "/clear", Description: "clear the conversation"},
	{Command: "/diff", Description: "show working-tree diff"},
}

var codexSlashCapabilities = []slashCapability{
	{Command: "/compact", Description: "condense the conversation"},
	{Command: "/status", Description: "show session configuration"},
	{Command: "/diff", Description: "show working-tree diff"},
	{Command: "/review", Description: "review current changes"},
	{Command: "/permissions", Description: "change approval mode"},
	{Command: "/model", Description: "change the active model"},
	{Command: "/new", Description: "start a new task"},
}

type harnessSnapshot struct {
	Messages       []histMsg
	LastReply      string
	LastActivity   int64
	Activity       activityInfo
	State          string
	Attention      attentionInfo
	Sequence       int64
	DegradedReason string
}

type deliveryResult struct {
	State string
}

var errTerminalOnly = errors.New("structured controls unavailable; use the terminal")

// harnessAdapter is the narrow harness-specific boundary. Endpoint and watcher
// code consume snapshots/capabilities instead of knowing transcript formats or
// keystroke semantics.
type harnessAdapter interface {
	Capabilities(context.Context, sessionInfo) sessionCapabilities
	Snapshot(context.Context, sessionInfo, string, bool) (harnessSnapshot, error)
	History(context.Context, sessionInfo) ([]histMsg, error)
	Attention(context.Context, sessionInfo, string, bool) (attentionInfo, error)
	Deliver(context.Context, sessionInfo, string, string, string, bool) (deliveryResult, error)
	Interrupt(context.Context, sessionInfo, string, bool) (bool, error)
	RespondAttention(context.Context, sessionInfo, attentionResponseRequest) (attentionResponseResult, error)
}

type genericAdapter struct{ s *server }

func (a genericAdapter) Capabilities(context.Context, sessionInfo) sessionCapabilities {
	return sessionCapabilities{}
}
func (a genericAdapter) Snapshot(context.Context, sessionInfo, string, bool) (harnessSnapshot, error) {
	return harnessSnapshot{State: "idle", DegradedReason: "unsupported harness"}, nil
}
func (a genericAdapter) History(context.Context, sessionInfo) ([]histMsg, error) { return nil, nil }
func (a genericAdapter) Attention(context.Context, sessionInfo, string, bool) (attentionInfo, error) {
	return attentionInfo{}, nil
}
func (a genericAdapter) Deliver(context.Context, sessionInfo, string, string, string, bool) (deliveryResult, error) {
	return deliveryResult{}, errTerminalOnly
}
func (a genericAdapter) Interrupt(context.Context, sessionInfo, string, bool) (bool, error) {
	return false, errTerminalOnly
}
func (a genericAdapter) RespondAttention(context.Context, sessionInfo, attentionResponseRequest) (attentionResponseResult, error) {
	return attentionResponseResult{}, errTerminalOnly
}

func (s *server) adapterFor(se sessionInfo) harnessAdapter {
	switch se.Tool {
	case "claude":
		return claudeAdapter{s: s}
	case "codex":
		return codexAdapter{s: s}
	default:
		return genericAdapter{s: s}
	}
}
