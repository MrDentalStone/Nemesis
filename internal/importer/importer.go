// Package importer talks to a deck importer server: rikrassen's MTG importer
// (https://importer.rikrassen.com) by default, the one the MTG tables ship
// with. It takes a decklist or a deck URL and streams back ready-to-spawn TTS
// objects as newline-delimited JSON. Any server with the same protocol works
// (pass its URL with -importer or NEMESIS_IMPORTER).
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://importer.rikrassen.com"

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Version string
}

func New(baseURL, version string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 100 * time.Second}, Version: version}
}

// Build posts data (a decklist or deck URL) and returns the ndjson pile lines
// plus any per-card problems.
// origin is the hand transform the server lays piles out from; the pad
// places every pile itself, so the table origin will do.
var origin = map[string]any{
	"position": map[string]float64{"x": 0, "y": 0, "z": 0},
	"rotation": map[string]float64{"x": 0, "y": 0, "z": 0},
	"scale":    map[string]float64{"x": 1, "y": 1, "z": 1},
	"forward":  map[string]float64{"x": 0, "y": 0, "z": 1},
	"right":    map[string]float64{"x": 1, "y": 0, "z": 0},
	"up":       map[string]float64{"x": 0, "y": 1, "z": 0},
}

func (c *Client) Build(ctx context.Context, data string) (piles []json.RawMessage, issues []string, err error) {
	body, _ := json.Marshal(map[string]any{"data": data, "useStates": true, "hand": origin})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/build", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("X-Client-Version", "Nemesis/"+c.Version)
	req.Header.Set("User-Agent", "Nemesis/"+c.Version)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("deck importer %s unreachable: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, nil, errors.New("deck importer: " + e.Error)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, `{"error":`) {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(line), &e) == nil {
				issues = append(issues, e.Error)
			}
			continue
		}
		piles = append(piles, json.RawMessage(line))
	}
	return piles, issues, nil
}

// FirstCard extracts a standalone single-card object from a pile (a Deck or a
// lone Card), so one token can be spawned on its own.
func FirstCard(pile json.RawMessage) (json.RawMessage, string, error) {
	var p struct {
		Name             string                     `json:"Name"`
		Nickname         string                     `json:"Nickname"`
		Description      string                     `json:"Description"`
		CardID           int                        `json:"CardID"`
		CustomDeck       map[string]json.RawMessage `json:"CustomDeck"`
		ContainedObjects []json.RawMessage          `json:"ContainedObjects"`
		States           json.RawMessage            `json:"States"`
	}
	if err := json.Unmarshal(pile, &p); err != nil {
		return nil, "", err
	}
	if p.Name == "Card" {
		return pile, p.Nickname, nil
	}
	if len(p.ContainedObjects) == 0 {
		return nil, "", errors.New("pile has no cards")
	}
	var card map[string]any
	if err := json.Unmarshal(p.ContainedObjects[0], &card); err != nil {
		return nil, "", err
	}
	// A contained card carries its CardID; it needs the matching sheet.
	id, _ := card["CardID"].(float64)
	sheet := fmt.Sprint(int(id) / 100)
	if s, ok := p.CustomDeck[sheet]; ok {
		card["CustomDeck"] = map[string]json.RawMessage{sheet: s}
	}
	card["Transform"] = map[string]float64{"posX": 0, "posY": 2, "posZ": 0, "rotX": 0, "rotY": 180, "rotZ": 0, "scaleX": 1, "scaleY": 1, "scaleZ": 1}
	out, err := json.Marshal(card)
	nick, _ := card["Nickname"].(string)
	return out, nick, err
}
