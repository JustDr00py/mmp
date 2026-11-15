package web

import (
	"cmp"
	"slices"

	"github.com/JustDr00py/mmp/internal/vodmeta"
)

// categoryLanguage is the language a category name is labelled with (see
// vodmeta.CategoryLanguage), or "".
func categoryLanguage(name string) string { return vodmeta.CategoryLanguage(name) }

// noLanguage stands for categories without a known label in filters.
const noLanguage = "-"

// langOption is one language of a filter, with how many categories have it.
type langOption struct {
	Value, Label string
	Categories   int
}

// languagesOf lists the languages names carry, most categories first, with
// unlabelled ones last.
func languagesOf(names []string) []langOption {
	count := map[string]int{}
	for _, n := range names {
		count[categoryLanguage(n)]++
	}
	var out []langOption
	for l, n := range count {
		if l != "" {
			out = append(out, langOption{Value: l, Label: l, Categories: n})
		}
	}
	slices.SortFunc(out, func(a, b langOption) int {
		return cmp.Or(cmp.Compare(b.Categories, a.Categories), cmp.Compare(a.Label, b.Label))
	})
	if n := count[""]; n > 0 {
		out = append(out, langOption{Value: noLanguage, Label: "No language label", Categories: n})
	}
	return out
}

// inLanguage reports whether a category name matches a filter value
// ("" matches everything).
func inLanguage(name, lang string) bool {
	switch lang {
	case "":
		return true
	case noLanguage:
		return categoryLanguage(name) == ""
	}
	return categoryLanguage(name) == lang
}
