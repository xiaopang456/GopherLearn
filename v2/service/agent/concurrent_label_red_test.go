package agent

import (
	"sync"
	"testing"
)

// BUG-AGENT-217 concurrency regression. Two governed searches in one turn can
// return [A,B] and [B,A] concurrently. The registry's atomic Episode card
// operation must still return one card prefix per Episode and must never
// overwrite a label. The two barriers force both goroutines through the first
// decision point before either is allowed to request the second Episode.
func TestConcurrentEpisodeCardAllocation(t *testing.T) {
	registry := NewMemoryEvidenceRegistry()
	orders := [][]string{{"episode-a", "episode-b"}, {"episode-b", "episode-a"}}
	versions := map[string]uint64{"episode-a": 1, "episode-b": 1}
	readyFirst := make(chan struct{}, 2)
	goFirst := make(chan struct{})
	readySecond := make(chan struct{}, 2)
	goSecond := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, order := range orders {
		order := order
		wg.Add(1)
		go func() {
			defer wg.Done()
			cards := map[string]string{}
			for index, episodeID := range order {
				if index == 0 {
					readyFirst <- struct{}{}
					<-goFirst
				} else {
					readySecond <- struct{}{}
					<-goSecond
				}
				cards[episodeID] = registry.GetOrAllocateEpisodeCard(episodeID, versions[episodeID])
			}
			for episodeID, card := range cards {
				for _, suffix := range []string{".s", ".d1"} {
					err := registry.RegisterAs(card+suffix, MemoryEvidenceEntry{
						Kind: MemoryEvidenceEpisodic, InternalID: episodeID + suffix,
						Version: versions[episodeID], Owner: 7, Status: "current",
						Source: "assertion:" + episodeID,
					})
					if err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
			}
		}()
	}
	<-readyFirst
	<-readyFirst
	close(goFirst)
	<-readySecond
	<-readySecond
	close(goSecond)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent governed registration returned errors: %v", errs)
	}

	aCard, ok := registry.EpisodeCard("episode-a", 1)
	if !ok {
		t.Fatal("episode-a has no card")
	}
	bCard, ok := registry.EpisodeCard("episode-b", 1)
	if !ok {
		t.Fatal("episode-b has no card")
	}
	if aCard == bCard {
		t.Fatalf("two Episodes share one card: %s", aCard)
	}
	if len(registry.Entries()) != 4 {
		t.Fatalf("expected synopsis+decision for two Episodes, got %d entries", len(registry.Entries()))
	}
	for _, episodeID := range []string{"episode-a", "episode-b"} {
		card, _ := registry.EpisodeCard(episodeID, 1)
		for _, suffix := range []string{".s", ".d1"} {
			entry, err := registry.Lookup(card + suffix)
			if err != nil || entry.InternalID != episodeID+suffix {
				t.Fatalf("%s%s is not stably bound: entry=%+v err=%v", card, suffix, entry, err)
			}
		}
	}
}
