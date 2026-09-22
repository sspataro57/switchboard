package main

// `opsctl slack-watch add|list|disable` (SWT-75 criterion 4): the human
// surface for the Slack watch list, mirroring `opsctl capture-rules` — the
// SPEC's named precedent for configuration that stays off the agent surface.
// Each subcommand is one executor call:
//
//	opsctl slack-watch add --workspace T0360B84U --conversation DSAV4HJ2F --label "José (DM)"
//	opsctl slack-watch list [--enabled true|false]
//	opsctl slack-watch disable --id 3     (and `enable --id 3` to turn it back on)
//
// There is no `remove`: a watch row is turned OFF, never deleted.

import (
	"encoding/json"
	"flag"
	"fmt"
)

func parseSlackWatch(sub string, argv []string) (string, json.RawMessage, error) {
	switch sub {
	case "add":
		fs := flag.NewFlagSet("slack-watch add", flag.ContinueOnError)
		workspace := fs.String("workspace", "", "Slack workspace id, T… (required)")
		conversation := fs.String("conversation", "", "conversation id, C… D… or G… (required)")
		label := fs.String("label", "", "who or what this is, for /sources and the list")
		if err := fs.Parse(argv); err != nil {
			return "", nil, err
		}
		if *workspace == "" || *conversation == "" {
			return "", nil, fmt.Errorf("--workspace and --conversation are required")
		}
		payload := map[string]any{"workspace_id": *workspace, "conversation_id": *conversation}
		if *label != "" {
			payload["label"] = *label
		}
		raw, err := json.Marshal(payload)
		return "slack_watch_add", raw, err
	case "disable", "enable":
		fs := flag.NewFlagSet("slack-watch "+sub, flag.ContinueOnError)
		id := fs.Int64("id", 0, "the watch row id, from `opsctl slack-watch list` (required)")
		if err := fs.Parse(argv); err != nil {
			return "", nil, err
		}
		if *id <= 0 {
			return "", nil, fmt.Errorf("--id is required")
		}
		raw, err := json.Marshal(map[string]any{"id": *id, "enabled": sub == "enable"})
		return "slack_watch_set_enabled", raw, err
	case "list":
		fs := flag.NewFlagSet("slack-watch list", flag.ContinueOnError)
		enabled := fs.String("enabled", "", "filter: true or false; omitted lists every row")
		if err := fs.Parse(argv); err != nil {
			return "", nil, err
		}
		payload := map[string]any{}
		switch *enabled {
		case "":
		case "true":
			payload["enabled"] = true
		case "false":
			payload["enabled"] = false
		default:
			return "", nil, fmt.Errorf("--enabled must be true or false")
		}
		raw, err := json.Marshal(payload)
		return "slack_watch_list", raw, err
	default:
		return "", nil, fmt.Errorf("usage: opsctl slack-watch <add|list|disable> [flags] (also: enable --id N; got %q)", sub)
	}
}
