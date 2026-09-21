package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ProgramSnapshot struct {
	Handle          string            `json:"handle"`
	Program         json.RawMessage   `json:"program"`
	Scopes          []json.RawMessage `json:"scopes"`
	ScopeExclusions []json.RawMessage `json:"scope_exclusions"`
	CapturedAt      time.Time         `json:"captured_at"`
}

type ReportSnapshot struct {
	ID              string          `json:"id"`
	Summary         json.RawMessage `json:"summary"`
	Report          json.RawMessage `json:"report"`
	DiscordThreadID string          `json:"discord_thread_id,omitempty"`
	CapturedAt      time.Time       `json:"captured_at"`
}

type State struct {
	Version             int                        `json:"version"`
	ProgramsInitialized bool                       `json:"programs_initialized"`
	ReportsInitialized  bool                       `json:"reports_initialized"`
	Programs            map[string]ProgramSnapshot `json:"programs"`
	Reports             map[string]ReportSnapshot  `json:"reports"`
	MissingPrograms     map[string]int             `json:"missing_programs,omitempty"`
}

func newState() State {
	return State{
		Version:         2,
		Programs:        make(map[string]ProgramSnapshot),
		Reports:         make(map[string]ReportSnapshot),
		MissingPrograms: make(map[string]int),
	}
}

type ProgramChange struct {
	Kind    string
	Handle  string
	Details []ChangeDetail
	Before  *ProgramSnapshot
	After   *ProgramSnapshot
}

type ReportChange struct {
	Kind    string
	ID      string
	Details []ChangeDetail
	Before  *ReportSnapshot
	After   *ReportSnapshot
}

type ChangeDetail struct {
	Path   string
	Before string
	After  string
}

func resourceID(raw json.RawMessage) (string, error) {
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if len(envelope.ID) == 0 || bytes.Equal(envelope.ID, []byte("null")) {
		return "", fmt.Errorf("resource has no id")
	}
	var s string
	if err := json.Unmarshal(envelope.ID, &s); err == nil {
		return s, nil
	}
	return strings.TrimSpace(string(envelope.ID)), nil
}

func programHandle(raw json.RawMessage) (string, error) {
	var envelope struct {
		Attributes struct {
			Handle string `json:"handle"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if envelope.Attributes.Handle == "" {
		return "", fmt.Errorf("program has no handle")
	}
	return envelope.Attributes.Handle, nil
}

func programState(raw json.RawMessage) (string, error) {
	var envelope struct {
		Attributes struct {
			State string `json:"state"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(envelope.Attributes.State)), nil

}

func programIsPrivate(raw json.RawMessage) (bool, error) {
	state, err := programState(raw)
	if err != nil {
		return false, err
	}
	switch state {
	case "soft_launched":
		return true, nil
	case "public_mode":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized state %q", state)
	}
}

func canonicalRaw(raw json.RawMessage) json.RawMessage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return b
}

func sortResources(resources []json.RawMessage) {
	sort.Slice(resources, func(i, j int) bool {
		left, _ := resourceID(resources[i])
		right, _ := resourceID(resources[j])
		return left < right
	})
}

func snapshotDigest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
