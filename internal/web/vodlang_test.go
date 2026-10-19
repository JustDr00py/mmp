package web

import "testing"

func TestCategoryLanguage(t *testing.T) {
	for name, want := range map[string]string{
		"EN | Action":           "English",
		"|FR| Films":            "French",
		"[DE] Serien":           "German",
		"ES: Comedia":           "Spanish",
		"(IT) Commedia":         "Italian",
		"VOD | NL | Actie":      "Dutch",
		"4K-EN Action":          "English",
		"ENGLISH MOVIES":        "English",
		"Peliculas Latino":      "Spanish",
		"Français - Drame":      "French",
		"Action (EN)":           "English",
		"Horror [AR]":           "Arabic",
		"Comedy - TR":           "Turkish",
		"UK | Drama":            "English",
		"AR | أفلام":            "Arabic",
		"Sci-Fi":                "",
		"IT Crowd Collection":   "",
		"Netflix Originals":     "",
		"Kids":                  "",
		"ACTION | Spider-Man":   "",
		"XX | Unknown code":     "",
		"  en | lower case ok ": "English",
	} {
		if got := categoryLanguage(name); got != want {
			t.Errorf("categoryLanguage(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLanguagesOf(t *testing.T) {
	got := languagesOf([]string{"EN | A", "EN | B", "FR | A", "Kids", "DE | A", "Netflix"})
	want := []langOption{
		{Value: "English", Label: "English", Categories: 2},
		{Value: "French", Label: "French", Categories: 1},
		{Value: "German", Label: "German", Categories: 1},
		{Value: noLanguage, Label: "No language label", Categories: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("languagesOf = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("languagesOf[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !inLanguage("Kids", noLanguage) || inLanguage("EN | A", noLanguage) || !inLanguage("EN | A", "") || inLanguage("FR | A", "English") {
		t.Error("inLanguage")
	}
}
