package validate

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// SplitResult is the outcome of a stratified split.
type SplitResult struct {
	Dev    []string
	Sealed []string
}

// SplitDevRatio is the share of questions that stay in the development split.
// The design asks for six to four.
const SplitDevRatio = 0.6

// Split assigns each question to the development or the sealed split.
//
// The split is stratified: questions are grouped by question type and language
// class, each group keeps the same six-to-four ratio, and the rounding
// leftovers go to the groups with the largest fractional parts. A plain random
// split would let a group with few questions end up entirely on one side,
// which would make the two splits incomparable.
//
// Inside a group the split keeps the topic and paraphrase mix as well, so the
// sealed split is not made of one kind of question. The design asks for the
// split to be stratified per layer, per type and per topic.
//
// The shuffle is seeded, so the same input always produces the same split: a
// frozen question set must not be re-cut between runs.
//
// The split is written into the question file's "split" field, so the caller
// has to write the result back out.
func Split(questions []dataset.Question, meta []Meta, seed int64) (SplitResult, error) {
	byID := map[string]Meta{}
	for _, m := range meta {
		byID[m.ID] = m
	}
	topicByID := map[string]string{}
	for _, q := range questions {
		topicByID[q.ID] = q.Topic
	}
	type group struct {
		key    string
		ids    []string
		devNum int
	}
	groups := map[string]*group{}
	order := []string{}
	for _, q := range questions {
		lang := ""
		layer := ""
		if m, ok := byID[q.ID]; ok {
			lang = m.Lang
			layer = m.Layer
		}
		key := layer + "|" + q.Type + "|" + lang
		g, ok := groups[key]
		if !ok {
			g = &group{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.ids = append(g.ids, q.ID)
	}
	sort.Strings(order)

	// Allocate the development share to each group, then fix the rounding.
	targetDev := int(math.Round(SplitDevRatio * float64(len(questions))))
	sizes := make([]int, len(order))
	for i, key := range order {
		sizes[i] = len(groups[key].ids)
	}
	groupDev := allocateDev(sizes, targetDev)
	for i, key := range order {
		groups[key].devNum = groupDev[i]
	}

	rng := rand.New(rand.NewSource(seed))
	result := SplitResult{}
	for gi, key := range order {
		g := groups[key]
		if g.devNum > len(g.ids) {
			return result, fmt.Errorf("split: group %q wants %d dev questions but has %d",
				g.key, g.devNum, len(g.ids))
		}
		// Inside the group, keep the topic and paraphrase mix: the sub-groups
		// get their own proportional share of the group's dev questions.
		subGroups := map[string][]string{}
		subOrder := []string{}
		for _, id := range g.ids {
			m := byID[id]
			subKey := m.Lang + "|" + m.Paraphrase + "|" + topicByID[id]
			if _, ok := subGroups[subKey]; !ok {
				subOrder = append(subOrder, subKey)
			}
			subGroups[subKey] = append(subGroups[subKey], id)
		}
		sort.Strings(subOrder)
		subSizes := make([]int, len(subOrder))
		for j, subKey := range subOrder {
			subSizes[j] = len(subGroups[subKey])
		}
		subDev := allocateDev(subSizes, groupDev[gi])

		for j, subKey := range subOrder {
			ids := subGroups[subKey]
			sort.Strings(ids)
			rng.Shuffle(len(ids), func(a, b int) { ids[a], ids[b] = ids[b], ids[a] })
			result.Dev = append(result.Dev, ids[:subDev[j]]...)
			result.Sealed = append(result.Sealed, ids[subDev[j]:]...)
		}
	}
	sort.Strings(result.Dev)
	sort.Strings(result.Sealed)
	return result, nil
}

// allocateDev hands out dev slots across strata in proportion to their size,
// using the largest remainder method.
//
// Every stratum gets the floor of its exact share, and the leftover slots go
// to the strata whose fractional part is the largest. The leftovers are handed
// out in descending fractional order, so a stratum never gets a slot that a
// stratum with a larger fractional part did not want.
//
// The returned slice sums to devTotal.
func allocateDev(sizes []int, devTotal int) []int {
	out := make([]int, len(sizes))
	fractions := make([]float64, len(sizes))
	assigned := 0
	for i, n := range sizes {
		exact := SplitDevRatio * float64(n)
		out[i] = int(math.Floor(exact))
		fractions[i] = exact - float64(out[i])
		assigned += out[i]
	}
	order := make([]int, len(sizes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return fractions[order[a]] > fractions[order[b]] })
	extra := 0
	for assigned < devTotal {
		out[order[extra%len(order)]]++
		assigned++
		extra++
	}
	for assigned > devTotal {
		// Take back from the stratum with the smallest fractional part first.
		for i := len(order) - 1; i >= 0; i-- {
			if out[order[i]] > 0 {
				out[order[i]]--
				assigned--
				break
			}
		}
	}
	return out
}
