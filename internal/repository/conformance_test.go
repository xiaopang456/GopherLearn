package repository_test

import (
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestInMemoryRepositoryContract runs the shared behavioural suite against the
// in-memory repository, so the store the development mode uses is held to the
// same message and citation rules as the GORM one.
func TestInMemoryRepositoryContract(t *testing.T) {
	repositorytest.Suite(t, func(*testing.T) repository.Repository { return repository.NewInMemoryRepository() })
}

// TestInMemoryMemoryContract holds the development store to the same long-term memory
// rules as MySQL, including the ones that are tempting to relax: a pending draft must not
// appear in a recallable listing, and deleting a session must not delete memories.
func TestInMemoryMemoryContract(t *testing.T) {
	repositorytest.MemorySuite(t, func(*testing.T) repository.Repository { return repository.NewInMemoryRepository() })
}

// TestInMemoryLearningContract holds the development store to the same quiz rules as
// MySQL: one open quiz per conversation, one grade per submission, a lease a
// superseded grader cannot write through, and a mastery projection rebuilt from the
// attempts that are actually stored.
func TestInMemoryLearningContract(t *testing.T) {
	repositorytest.LearningSuite(t, func(*testing.T) repository.Repository { return repository.NewInMemoryRepository() })
}

// TestInMemoryTopicMemoryContract holds the development store to the same consent rules as
// MySQL: no row means off, repeating an answer changes nothing, and a foreign conversation is
// indistinguishable from one that never existed.
func TestInMemoryTopicMemoryContract(t *testing.T) {
	repositorytest.TopicMemorySuite(t, func(*testing.T) repository.Repository { return repository.NewInMemoryRepository() })
}

// TestInMemoryEpisodeContract holds the development store to the same episodic
// memory rules as MySQL: one versioned card per session under optimistic
// concurrency, durable jobs a crashed worker cannot duplicate, and a candidate
// reload that refuses anything the authority no longer backs.
func TestInMemoryEpisodeContract(t *testing.T) {
	repositorytest.EpisodeSuite(t, func(*testing.T) repository.Repository { return repository.NewInMemoryRepository() })
}
