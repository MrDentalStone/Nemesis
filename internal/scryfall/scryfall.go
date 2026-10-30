// Package scryfall is a small, polite client for the Scryfall API: a
// descriptive User-Agent, an Accept header and at most ~8 requests a second,
// as https://scryfall.com/docs/api asks.
package scryfall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const base = "https://api.scryfall.com"

type Client struct {
	HTTP      *http.Client
	UserAgent string
	mu        sync.Mutex
	last      time.Time
}

func New(version string) *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "Nemesis/" + version + " (+https://github.com/MrDentalStone/Nemesis)",
	}
}

type Card struct {
	Name            string            `json:"name"`
	Set             string            `json:"set"`
	CollectorNumber string            `json:"collector_number"`
	TypeLine        string            `json:"type_line"`
	ColorIdentity   []string          `json:"color_identity"`
	Legalities      map[string]string `json:"legalities"`
	GameChanger     bool              `json:"game_changer"`
	Power           string            `json:"power"`
	Toughness       string            `json:"toughness"`
	OracleText      string            `json:"oracle_text"`
	ManaCost        string            `json:"mana_cost"`
	Rarity          string            `json:"rarity"`
	CardFaces       []struct {
		Name     string `json:"name"`
		ManaCost string `json:"mana_cost"`
	} `json:"card_faces"`
}

// Cost is the card's mana cost; a double-faced card's is its front face's.
func (c Card) Cost() string {
	if c.ManaCost == "" && len(c.CardFaces) > 0 {
		return c.CardFaces[0].ManaCost
	}
	return c.ManaCost
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	c.mu.Lock()
	if wait := 125*time.Millisecond - time.Since(c.last); wait > 0 {
		time.Sleep(wait)
	}
	c.last = time.Now()
	c.mu.Unlock()

	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Details string `json:"details"`
		}
		json.Unmarshal(raw, &e)
		if e.Details == "" {
			e.Details = resp.Status
		}
		return &Error{Status: resp.StatusCode, Msg: e.Details}
	}
	return json.Unmarshal(raw, out)
}

type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("scryfall: %s", e.Msg) }

// Search returns the first page of a Scryfall search.
func (c *Client) Search(ctx context.Context, q, order string) ([]Card, error) {
	v := url.Values{"q": {q}}
	if order != "" {
		v.Set("order", order)
	}
	var page struct {
		Data []Card `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/cards/search?"+v.Encode(), nil, &page); err != nil {
		var se *Error
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return page.Data, nil
}

// SearchAll follows a search through every page (at most pages of them).
func (c *Client) SearchAll(ctx context.Context, q string, pages int) ([]Card, error) {
	path := "/cards/search?" + url.Values{"q": {q}}.Encode()
	var all []Card
	for i := 0; i < pages && path != ""; i++ {
		var page struct {
			Data     []Card `json:"data"`
			HasMore  bool   `json:"has_more"`
			NextPage string `json:"next_page"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			var se *Error
			if errors.As(err, &se) && se.Status == http.StatusNotFound {
				return all, nil
			}
			return nil, err
		}
		all = append(all, page.Data...)
		path = ""
		if page.HasMore {
			path = strings.TrimPrefix(page.NextPage, base)
		}
	}
	return all, nil
}

// Collection looks cards up by exact name, 75 per request. found maps each
// requested name to its card; missing lists names Scryfall did not know.
func (c *Client) Collection(ctx context.Context, names []string) (found map[string]Card, missing []string, err error) {
	found = map[string]Card{}
	for i := 0; i < len(names); i += 75 {
		chunk := names[i:min(i+75, len(names))]
		ids := make([]map[string]string, len(chunk))
		for j, n := range chunk {
			ids[j] = map[string]string{"name": n}
		}
		var res struct {
			Data     []Card `json:"data"`
			NotFound []struct {
				Name string `json:"name"`
			} `json:"not_found"`
		}
		if err := c.do(ctx, http.MethodPost, "/cards/collection", map[string]any{"identifiers": ids}, &res); err != nil {
			return nil, nil, err
		}
		miss := map[string]bool{}
		for _, nf := range res.NotFound {
			miss[nf.Name] = true
			missing = append(missing, nf.Name)
		}
		// results come back in request order, minus the misses
		k := 0
		for _, n := range chunk {
			if miss[n] || k >= len(res.Data) {
				continue
			}
			found[n] = res.Data[k]
			k++
		}
	}
	return found, missing, nil
}
