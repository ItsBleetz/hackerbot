package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

const (
	dummyProgramLimit = 3
	dummyReportLimit  = 2
)

// sendDummyNotifications is a live, bounded Discord preview. It reads a small
// subset from HackerOne, but deliberately receives no StateStore and therefore
// cannot read or alter the SQLite monitoring baseline.
func sendDummyNotifications(ctx context.Context, cfg Config) error {
	h1 := newH1Client(cfg)
	discord := newDiscordClient(cfg.timeout)
	monitor := &Monitor{cfg: cfg, h1: h1, discord: discord}

	programs, err := h1.ProgramsLimited(ctx, dummyProgramLimit)
	if err != nil {
		return fmt.Errorf("list sample programs: %w", err)
	}
	for index, raw := range programs {
		if index > 0 {
			if err := discord.SendProgramDivider(ctx, cfg.ProgramWebhookURL); err != nil {
				return fmt.Errorf("send divider before sample program %d: %w", index+1, err)
			}
		}
		handle, err := programHandle(raw)
		if err != nil {
			return fmt.Errorf("decode sample program %d: %w", index+1, err)
		}
		snapshot, err := monitor.FetchProgram(ctx, handle)
		if err != nil {
			return err
		}
		if err := discord.SendProgram(ctx, cfg.ProgramWebhookURL, ProgramChange{
			Kind: "manual", Handle: handle, After: &snapshot,
		}); err != nil {
			return fmt.Errorf("send sample program %s: %w", handle, err)
		}
		log.Printf("dummy test sent program %d of %d: %s", index+1, len(programs), handle)
	}

	reports, err := h1.ReportsLimited(ctx, dummyReportLimit)
	if err != nil {
		return fmt.Errorf("list sample reports: %w", err)
	}
	for index, summary := range reports {
		id, err := resourceID(summary)
		if err != nil {
			return fmt.Errorf("decode sample report %d: %w", index+1, err)
		}
		report, err := h1.Report(ctx, id)
		if err != nil {
			return fmt.Errorf("fetch sample report %s: %w", id, err)
		}
		if err := sendDummyReport(ctx, discord, cfg, id, summary, report); err != nil {
			return fmt.Errorf("send sample report %s: %w", id, err)
		}
		log.Printf("dummy test sent report %d of %d: %s", index+1, len(reports), id)
	}
	return nil
}

func sendDummyReport(ctx context.Context, discord *DiscordClient, cfg Config, id string, summary, report json.RawMessage) error {
	activities := append([]json.RawMessage(nil), relationshipResources(report, "activities")...)
	sort.SliceStable(activities, func(i, j int) bool {
		return activityCreatedAt(activities[i]).Before(activityCreatedAt(activities[j]))
	})

	emptyReport, err := reportWithActivities(report, nil)
	if err != nil {
		return err
	}
	current := ReportSnapshot{
		ID: id, Summary: summary, Report: emptyReport, CapturedAt: time.Now().UTC(),
	}
	threadID, err := discord.SendReport(ctx, cfg.ReportWebhookURL, ReportChange{
		Kind: "preview", ID: id, After: &current,
	}, cfg.ReportNotificationMode, cfg.ReportNotifyOwnComments, cfg.ReportThreadsEnabled)
	if err != nil {
		return err
	}
	if len(activities) == 0 {
		return nil
	}

	completeReport, err := reportWithActivities(report, activities)
	if err != nil {
		return err
	}
	before := current
	after := ReportSnapshot{
		ID: id, Summary: summary, Report: completeReport, DiscordThreadID: threadID, CapturedAt: time.Now().UTC(),
	}
	_, err = discord.SendReport(ctx, cfg.ReportWebhookURL, ReportChange{
		Kind: "changed", ID: id, Before: &before, After: &after,
	}, cfg.ReportNotificationMode, cfg.ReportNotifyOwnComments, cfg.ReportThreadsEnabled)
	return err
}

func activityCreatedAt(activity json.RawMessage) time.Time {
	value := stringValue(objectAttributes(activity)["created_at"])
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func reportWithActivities(report json.RawMessage, activities []json.RawMessage) (json.RawMessage, error) {
	var root map[string]any
	if err := json.Unmarshal(report, &root); err != nil {
		return nil, fmt.Errorf("decode report activities: %w", err)
	}
	relationships, ok := root["relationships"].(map[string]any)
	if !ok {
		relationships = make(map[string]any)
		root["relationships"] = relationships
	}
	items := make([]any, 0, len(activities))
	for _, activity := range activities {
		var item any
		if err := json.Unmarshal(activity, &item); err != nil {
			return nil, fmt.Errorf("decode report activity: %w", err)
		}
		items = append(items, item)
	}
	relationships["activities"] = map[string]any{"data": items}
	result, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode report activities: %w", err)
	}
	return result, nil
}
