package markup_test

import (
	"testing"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestFactOfPinsColumnToFieldMapping(t *testing.T) {
	req := markup.Request{
		ProductID:    "p-1",
		Category:     "books",
		CustomerTier: "enterprise",
		Channel:      "web",
		Country:      "BR",
		Inventory:    "in_stock",
		TimeWindow:   "peak",
		Amount:       42.5,
	}
	fact := markup.FactOf(req)
	cases := []struct {
		key  string
		want string
	}{
		{"product_id", "p-1"},
		{"category", "books"},
		{"customer_tier", "enterprise"},
		{"channel", "web"},
		{"country", "BR"},
		{"inventory", "in_stock"},
		{"time_window", "peak"},
	}
	for _, tc := range cases {
		got, ok := fact[tc.key]
		if !ok {
			t.Errorf("fact[%q] missing", tc.key)
			continue
		}
		s, ok := got.(string)
		if !ok {
			t.Errorf("fact[%q] = %T, want string", tc.key, got)
			continue
		}
		if s != tc.want {
			t.Errorf("fact[%q] = %q, want %q", tc.key, s, tc.want)
		}
	}
}

func TestFactOfOmitsAmount(t *testing.T) {
	fact := markup.FactOf(markup.Request{Amount: 99.99})
	if _, present := fact["amount"]; present {
		t.Fatal("FactOf must not expose Amount: parser grammar is string/set only")
	}
	if _, present := fact["Amount"]; present {
		t.Fatal("FactOf must not expose Amount (capitalized): parser grammar is string/set only")
	}
}

func TestFactOfZeroRequestProducesEmptyStrings(t *testing.T) {
	fact := markup.FactOf(markup.Request{})
	for _, key := range []string{"product_id", "category", "customer_tier", "channel", "country", "inventory", "time_window"} {
		got, ok := fact[key]
		if !ok {
			t.Errorf("fact[%q] missing for zero Request", key)
			continue
		}
		if s, _ := got.(string); s != "" {
			t.Errorf("fact[%q] = %q, want empty string", key, s)
		}
	}
}
