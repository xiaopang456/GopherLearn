package app

import (
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// MemoryAccessConfig is the deployment's decision about the memory read side, and the only
// field in this assembly that a caller cannot talk its way into later.
//
// The zero value is today's production behaviour: global memories plus the requesting
// session's own episodes, and no topic memory at all. A deployment turns topics on by
// setting TopicMemory where the assembly is built, next to the credentials and the provider
// selection - which is the same place every other permission of this process is decided.
type MemoryAccessConfig struct {
	// TopicMemory installs the server-derived topic authorizer. Installing it grants nothing on its
	// own: the learner has to have switched this conversation on, and this conversation has to hold
	// a live quiz naming the one subject the switch applies to. That is why a deployment can turn
	// this on without becoming responsible for somebody else's consent - the second condition is
	// stored per conversation and read again on every recall, so no assembly decision can outrun it
	// and taking the consent back takes the read away on the next turn.
	TopicMemory bool
	// Search bounds one recall. The zero value normalizes to the production caps and the
	// 0.6 confidence floor, so leaving it alone changes nothing.
	Search service.MemorySearchOptions
}

func memoryAuthorizer(store repository.Repository, config MemoryAccessConfig) (service.MemoryAuthorizer, error) {
	authorizer := service.MemoryAuthorizer(service.SessionMemoryGrant{})
	if config.TopicMemory {
		grant, err := service.NewLearningTopicGrant(store)
		if err != nil {
			return nil, err
		}
		authorizer = grant
	}
	return authorizer, nil
}

// NewMemoryContextSupply builds the learning-agent window supply with the same
// authorizer used by the memory tool path.
func NewMemoryContextSupply(store repository.Repository, config MemoryAccessConfig) (*service.MemoryContextSupply, error) {
	authorizer, err := memoryAuthorizer(store, config)
	if err != nil {
		return nil, err
	}
	return service.NewMemoryContextSupply(store, authorizer, config.Search)
}

//
// It exists so the opt-in has exactly one door. Handing a deployment
// service.NewLearningTopicGrant directly would mean the base grant, the store and the
// bounds are assembled by whoever happened to want the feature; this keeps the pairing with
// repository.Repository in one place and makes "off" the shape a caller gets by not asking.
func NewMemoryAccess(store repository.Repository, config MemoryAccessConfig) (*service.MemoryAccess, error) {
	authorizer, err := memoryAuthorizer(store, config)
	if err != nil {
		return nil, err
	}
	return service.NewMemoryAccess(store, authorizer, config.Search)
}
