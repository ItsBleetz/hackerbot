package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

func diffProgram(before, after ProgramSnapshot) []ChangeDetail {
	var details []ChangeDetail
	diffJSON("program", before.Program, after.Program, &details)
	diffResourceSets("scope", before.Scopes, after.Scopes, &details)
	diffResourceSets("scope_exclusion", before.ScopeExclusions, after.ScopeExclusions, &details)
	return details
}

func diffReport(before, after ReportSnapshot) []ChangeDetail {
	var details []ChangeDetail
	diffJSON("report", stripRelationship(before.Report, "activities"), stripRelationship(after.Report, "activities"), &details)
	diffResourceSets("activity", relationshipResources(before.Report, "activities"), relationshipResources(after.Report, "activities"), &details)
	return details
}

func diffJSON(prefix string, before, after json.RawMessage, details *[]ChangeDetail) {
	var left, right any
	if json.Unmarshal(before, &left) != nil || json.Unmarshal(after, &right) != nil {
		if !reflect.DeepEqual(before, after) {
			appendDetail(details, prefix, string(before), string(after))
		}
		return
	}
	diffValue(prefix, left, right, details)
}

func diffValue(path string, before, after any, details *[]ChangeDetail) {
	if reflect.DeepEqual(before, after) {
		return
	}
	leftMap, leftOK := before.(map[string]any)
	rightMap, rightOK := after.(map[string]any)
	if leftOK && rightOK {
		keys := make(map[string]struct{}, len(leftMap)+len(rightMap))
		for key := range leftMap {
			keys[key] = struct{}{}
		}
		for key := range rightMap {
			keys[key] = struct{}{}
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		for _, key := range ordered {
			diffValue(path+"."+key, leftMap[key], rightMap[key], details)
		}
		return
	}
	appendDetail(details, path, compactValue(before), compactValue(after))
}

func diffResourceSets(prefix string, before, after []json.RawMessage, details *[]ChangeDetail) {
	left := resourcesByID(before)
	right := resourcesByID(after)
	keys := make(map[string]struct{}, len(left)+len(right))
	for id := range left {
		keys[id] = struct{}{}
	}
	for id := range right {
		keys[id] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for id := range keys {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		oldRaw, oldOK := left[id]
		newRaw, newOK := right[id]
		switch {
		case !oldOK:
			appendDetail(details, prefix+"["+id+"]", "(not present)", compactJSON(newRaw))
		case !newOK:
			appendDetail(details, prefix+"["+id+"]", compactJSON(oldRaw), "(removed)")
		default:
			diffJSON(prefix+"["+id+"]", oldRaw, newRaw, details)
		}
	}
}

func resourcesByID(items []json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(items))
	for index, item := range items {
		id, err := resourceID(item)
		if err != nil || id == "" {
			id = strconv.Itoa(index)
		}
		result[id] = item
	}
	return result
}

func relationshipResources(raw json.RawMessage, name string) []json.RawMessage {
	var root struct {
		Relationships map[string]struct {
			Data json.RawMessage `json:"data"`
		} `json:"relationships"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	var resources []json.RawMessage
	if json.Unmarshal(root.Relationships[name].Data, &resources) != nil {
		return nil
	}
	return resources
}

func stripRelationship(raw json.RawMessage, name string) json.RawMessage {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	if relationships, ok := root["relationships"].(map[string]any); ok {
		delete(relationships, name)
	}
	b, _ := json.Marshal(root)
	return b
}

func appendDetail(details *[]ChangeDetail, path, before, after string) {
	*details = append(*details, ChangeDetail{Path: path, Before: before, After: after})
}

func compactJSON(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return strings.TrimSpace(string(raw))
	}
	return compactValue(value)
}

func compactValue(value any) string {
	if value == nil {
		return "null"
	}
	if text, ok := value.(string); ok {
		return text
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(b)
}

func abbreviate(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}
