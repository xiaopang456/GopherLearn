package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// MemoryAccess is the read side of long-term memory: it decides, on the server, which
// confirmed memories one caller may be shown. It is deliberately not a thinner wrapper
// over the repository - the repository already refuses to list anything but active rows,
// and this layer adds the part that cannot be stored in a status column: which subjects
// this session was granted, how good a match has to be to be worth a model's attention,
// and how much text one read may carry.
//
// The recall is lexical and deterministic. It is not a vector search and is not presented
// as one: a memory table this small gains nothing from embeddings, and a claim about
// semantic recall would be a claim about a capability this code does not have.
type MemoryAccess struct {
	repo       repository.Repository
	authorizer MemoryAuthorizer
	options    MemorySearchOptions
}

// MemoryAuthorizer answers "what may this session remember". It is a seam rather than a
// parameter because the answer must never come from the caller who is asking - and in a
// tool loop, the caller asking is a model.
type MemoryAuthorizer interface {
	// AuthorizeMemoryScope returns the grant for one authenticated user in one session.
	// A returned error ends the read; an empty grant is not an error, it is "nothing is
	// authorized", and the caller reports that honestly instead of searching wider.
	AuthorizeMemoryScope(ctx context.Context, userID, sessionID string) (MemoryGrant, error)
}

// MemoryGrant is the server-generated authorization surface of one read.
//
// The zero grant allows nothing, which is the safe default for a system whose topic
// consent store does not exist yet: forgetting to wire an authorizer must not become a
// reason to read every topic the user has ever confirmed.
type MemoryGrant struct {
	// AllowGlobal opens the user's global memories - the stable facts and preferences
	// they confirmed for their whole account.
	AllowGlobal bool
	// TopicKeys lists the subjects whose topic memory may be read. A topic memory whose
	// key is not here is invisible, and no request field can add a key: the model is not
	// the party that decides this.
	TopicKeys []string
	// SessionIDs limits episodic memory to the listed conversations. An episode is the
	// record of what happened in a specific session, so reading one from another session
	// needs naming that session here - by the server.
	SessionIDs []string
}

// Empty reports whether the grant allows nothing at all.
func (g MemoryGrant) Empty() bool {
	return !g.AllowGlobal && len(normalizeKeys(g.TopicKeys)) == 0 && len(normalizeKeys(g.SessionIDs)) == 0
}

// SessionMemoryGrant is the default authorizer: a user's global memories, plus the
// episodes of the session making the request, and no topic memory.
//
// That combination is not a convenience. Global memories are facts the user confirmed
// about themselves for their whole account, and the current session's episodes are
// already inside the conversation being summarized - so neither widens what a caller can
// learn beyond what the user chose to share with this product. Topic memory is the one
// layer that needs explicit per-subject consent, and there is no consent store yet, so
// this authorizer grants none.
type SessionMemoryGrant struct{}

// AuthorizeMemoryScope implements MemoryAuthorizer.
func (SessionMemoryGrant) AuthorizeMemoryScope(_ context.Context, userID, sessionID string) (MemoryGrant, error) {
	if strings.TrimSpace(userID) == "" {
		return MemoryGrant{}, ErrUnauthorized
	}
	grant := MemoryGrant{AllowGlobal: true}
	if strings.TrimSpace(sessionID) != "" {
		grant.SessionIDs = []string{sessionID}
	}
	return grant, nil
}

// MemorySearchOptions bounds one recall. Each cap exists because a memory read is fed
// into a prompt, and the size of a prompt is this server's responsibility regardless of
// how much the user happened to confirm.
type MemorySearchOptions struct {
	// MaxResults caps how many memories one read returns.
	MaxResults int
	// MinConfidence is the floor below which a memory is not shown. It is a recall
	// threshold, not a storage rule: a low-confidence memory stays confirmable and
	// correctable, it simply is not treated as worth the model's attention. A configured
	// value may only raise the default - see DefaultMemoryMinConfidence - because
	// "active" is not the same thing as "visible".
	MinConfidence float64
	// ItemRunes caps the text of one memory inside the result.
	ItemRunes int
	// TotalRunes caps the whole recalled text across all hits.
	TotalRunes int
	// QueryRunes caps the search text a caller may supply.
	QueryRunes int
}

const (
	DefaultMemoryMaxResults = 3
	DefaultMemoryItemRunes  = 600
	DefaultMemoryTotalRunes = 2400
	DefaultMemoryQueryRunes = 256
	// DefaultMemoryMinConfidence is the production floor: 0.6, the filter strength the
	// earlier long-term-memory pairing experiment measured as M2. It is not a stylistic
	// number - below it a memory is a guess, and a guess put in front of a model becomes
	// a fact the answer is built from. There is deliberately no way to switch the filter
	// off: 0 in a request means "unset", never "filter nothing".
	DefaultMemoryMinConfidence = 0.6
)

// Normalize fills in working defaults, so a zero configuration searches something rather
// than silently becoming unbounded or empty.
func (o MemorySearchOptions) Normalize() MemorySearchOptions {
	if o.MaxResults <= 0 || o.MaxResults > 10 {
		o.MaxResults = DefaultMemoryMaxResults
	}
	// An unusable configured floor - including NaN and either infinity, both of which
	// compare false against 0 and 1 - becomes the default rather than a filter that
	// silently never fires. And a valid one may only raise the bar, never lower it.
	if !validMemoryConfidence(o.MinConfidence) || o.MinConfidence < DefaultMemoryMinConfidence {
		o.MinConfidence = DefaultMemoryMinConfidence
	}
	if o.ItemRunes <= 0 || o.ItemRunes > 2000 {
		o.ItemRunes = DefaultMemoryItemRunes
	}
	if o.TotalRunes <= 0 || o.TotalRunes > 8000 {
		o.TotalRunes = DefaultMemoryTotalRunes
		if o.ItemRunes*o.MaxResults > o.TotalRunes {
			// Keep the per-item cap consistent with the total: three items that cannot
			// fit are a truncation the caller has to rediscover on every call.
			o.ItemRunes = o.TotalRunes / o.MaxResults
		}
	}
	if o.QueryRunes <= 0 || o.QueryRunes > 512 {
		o.QueryRunes = DefaultMemoryQueryRunes
	}
	return o
}

var (
	// ErrNoMemoryStore reports a recall attempted without a store. Refused at
	// construction, so it should not be reachable.
	ErrNoMemoryStore = errors.New("service: memory recall has no store")
	// ErrNoMemoryAuthorizer reports a recall with nobody to decide the grant. An
	// absent authorizer must not be read as "allow everything".
	ErrNoMemoryAuthorizer = errors.New("service: memory recall has no authorizer")
	// ErrMemoryQueryInvalid reports an empty or over-long search text.
	ErrMemoryQueryInvalid = errors.New("service: memory query is empty or too long")
	// ErrMemoryConfidenceInvalid reports a requested floor that is not a number between
	// 0 and 1. It is refused rather than normalized because a caller that sent NaN, an
	// infinity or 1.5 does not know what it asked for, and guessing on its behalf would
	// put a filter setting in front of the model that nobody chose.
	ErrMemoryConfidenceInvalid = errors.New("service: memory confidence floor is not a number between 0 and 1")
)

// validMemoryConfidence accepts exactly [0, 1]. The plain double comparison is enough for
// the three cases that look harmless and are not: NaN fails both sides of it, and each
// infinity fails one.
func validMemoryConfidence(value float64) bool {
	return value >= 0 && value <= 1
}

// MemoryHit is one recalled memory plus the internal reference a server-side caller needs
// and a model never sees. The reference carries the identifiers that make a memory
// checkable - which row, which subject, which conversation and messages it came from -
// because "where did this come from" must stay answerable after the text is put into a
// prompt. Those fields are intentionally absent from the rendered result: an identifier a
// model can read is an identifier a model can invent.
type MemoryHit struct {
	Memory    domain.Memory
	Reference MemoryReference
	// Content is the memory text after the per-item length cap, which is exactly what
	// the caller is allowed to put in front of the model.
	Content string
	// Score is the lexical match strength, 0 to 1, deterministic and comparable only
	// inside one call. It is not a probability and not a relevance claim about meaning.
	Score float64
	// MatchedTokens counts the distinct query terms this memory contains.
	MatchedTokens int
}

// MemoryReference is the server-internal handle for one hit.
type MemoryReference struct {
	MemoryID         string
	Scope            domain.MemoryScope
	TopicKey         string
	SourceSessionID  string
	SourceMessageIDs []string
	Confidence       float64
	// Citation is the source record a turn may attach to an answer. It is produced here,
	// from the stored row, so no caller - and no model - composes an identifier.
	Citation domain.Source
}

// MemoryQuery is one recall request. UserID and SessionID are the authenticated facts of
// the turn, not filters chosen by the caller of the read. MaxResults and MinConfidence
// narrow within the configured bounds and may not widen them: a zero MinConfidence means
// "no preference", so the default floor applies, and there is no value that turns the
// filter off.
type MemoryQuery struct {
	UserID        string
	SessionID     string
	Query         string
	MaxResults    int
	MinConfidence float64
}

// NewMemoryAccess wires the read side. A missing store or authorizer is refused here,
// where it can still stop startup, rather than at first use where it would look like an
// empty memory table.
func NewMemoryAccess(repo repository.Repository, authorizer MemoryAuthorizer, options MemorySearchOptions) (*MemoryAccess, error) {
	if repo == nil {
		return nil, ErrNoMemoryStore
	}
	if authorizer == nil {
		return nil, ErrNoMemoryAuthorizer
	}
	return &MemoryAccess{repo: repo, authorizer: authorizer, options: options.Normalize()}, nil
}

// Recall reads the memories this session is entitled to, in a deterministic order. The
// third return value is a scope note describing what was searched, which is what lets an
// empty result be reported honestly as "nothing in the authorized scope matched" rather
// than as "you have no memories".
func (a *MemoryAccess) Recall(ctx context.Context, query MemoryQuery) ([]MemoryHit, string, error) {
	if ctx == nil {
		return nil, "", ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	userID := strings.TrimSpace(query.UserID)
	if userID == "" {
		return nil, "", ErrUnauthorized
	}
	text := strings.TrimSpace(query.Query)
	if text == "" || len([]rune(text)) > a.options.QueryRunes {
		return nil, "", ErrMemoryQueryInvalid
	}

	// Only a legal floor may be honoured at all: 1.5, NaN and both infinities are refused
	// rather than guessed at, because silently clamping them would be a filter setting this
	// server invented on the caller's behalf. Checked before any read, so a malformed
	// request cannot even reach the authorization step.
	if !validMemoryConfidence(query.MinConfidence) {
		return nil, "", ErrMemoryConfidenceInvalid
	}

	grant, err := a.authorizer.AuthorizeMemoryScope(ctx, userID, strings.TrimSpace(query.SessionID))
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	if grant.Empty() {
		// Nothing is authorized. Searching everything anyway would be the one mistake
		// this layer exists to prevent, so the answer is an honest empty result.
		return []MemoryHit{}, "本轮没有任何记忆检索授权，因此没有读取记忆。", nil
	}

	candidates, note, err := a.collect(ctx, userID, grant)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return []MemoryHit{}, note, nil
	}

	queryTokens := retrieval.Tokenize(strings.ToLower(text))
	if len(queryTokens) == 0 {
		return []MemoryHit{}, note, nil
	}
	// Tighten only: the configured floor already sits at or above the 0.6 default, so the
	// effective floor is the higher of the two and no request can lower it. Zero is the
	// absence of a preference, not "show everything".
	floor := a.options.MinConfidence
	if query.MinConfidence > floor {
		floor = query.MinConfidence
	}
	limit := a.options.MaxResults
	if query.MaxResults > 0 && query.MaxResults < limit {
		limit = query.MaxResults
	}

	type scored struct {
		hit   MemoryHit
		order int
	}
	matched := make([]scored, 0, len(candidates))
	for index, memory := range candidates {
		if memory.Confidence < floor {
			continue
		}
		hits, total := overlap(queryTokens, memory.Content)
		if (hits == 0 || total == 0) && memory.Scope != domain.MemoryScopeGlobal {
			// No shared term at all: for topic and episodic records, where a
			// recall is supposed to be evidence of relevance, the record is not
			// shown because it exists.
			//
			// Global records are exempt, and that is a correction with a
			// source: the original project carried the user's confirmed profile
			// into every window without asking the question first, so a profile
			// fact like "我住在成都" was always available even when the question
			// shared no word with it. A lexical scorer cannot see that
			// relevance, and without the exemption it never surfaces the fact
			// at all. The exemption only fills the slots left over after the
			// matching records: they rank by the same deterministic order, at
			// score zero.
			continue
		}
		matched = append(matched, scored{
			hit: MemoryHit{
				Memory:        memory,
				Reference:     referenceFor(memory),
				Content:       clipRunes(memory.Content, a.options.ItemRunes),
				Score:         float64(hits) / float64(total),
				MatchedTokens: hits,
			},
			order: index,
		})
	}
	// Deterministic order, decided by data rather than by whatever the engine returned
	// first: score, then the memory's own creation time, then the position the store gave
	// it. The last one matters more than it looks. A memory id is not a usable
	// tie-breaker across stores - the SQL one embeds a fixed-width timestamp and sorts like
	// a clock, the development one is a decimal counter where "mem-9" sorts after
	// "mem-10" - while both stores list in creation order by contract, so that order is
	// the only tie-break the two can agree on.
	sort.SliceStable(matched, func(left, right int) bool {
		a, b := matched[left], matched[right]
		if a.hit.Score != b.hit.Score {
			return a.hit.Score > b.hit.Score
		}
		if !a.hit.Memory.CreatedAt.Equal(b.hit.Memory.CreatedAt) {
			return a.hit.Memory.CreatedAt.Before(b.hit.Memory.CreatedAt)
		}
		return a.order < b.order
	})

	found := make([]MemoryHit, 0, len(matched))
	used := 0
	for _, entry := range matched {
		if len(found) >= limit {
			break
		}
		hit := entry.hit
		// The total bound lives here and nowhere else. The clipped form carries an
		// ellipsis, so it is cut to one rune less than the space left, and a remainder too
		// small for one rune plus the mark ends the read: half a memory is not a memory,
		// and it would earn a citation for text nobody was shown.
		if remaining := a.options.TotalRunes - used; len([]rune(hit.Content)) > remaining {
			if remaining < 2 {
				break
			}
			hit.Content = clipRunes(hit.Content, remaining-1)
		}
		used += len([]rune(hit.Content))
		found = append(found, hit)
	}
	return found, note, nil
}

// collect gathers the active memories the grant covers. Every read goes through the
// repository's status-filtered listing, one call per authorized subject, so "active only"
// is enforced by the store rather than re-implemented here and gotten wrong.
func (a *MemoryAccess) collect(ctx context.Context, userID string, grant MemoryGrant) ([]domain.Memory, string, error) {
	collected := make([]domain.Memory, 0)
	seen := make(map[string]struct{})
	appendMemories := func(items []domain.Memory) {
		for _, memory := range items {
			if _, duplicate := seen[memory.MemoryID]; duplicate {
				continue
			}
			seen[memory.MemoryID] = struct{}{}
			collected = append(collected, memory)
		}
	}

	if grant.AllowGlobal {
		items, err := a.repo.ListActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
		if err != nil {
			return nil, "", mapRepositoryError(err)
		}
		appendMemories(items)
	}
	for _, key := range normalizeKeys(grant.TopicKeys) {
		items, err := a.repo.ListActiveMemories(ctx, userID, domain.MemoryScopeTopic, key)
		if err != nil {
			return nil, "", mapRepositoryError(err)
		}
		appendMemories(items)
	}
	grantedSessions := normalizeKeys(grant.SessionIDs)
	if len(grantedSessions) > 0 {
		items, err := a.repo.ListActiveMemories(ctx, userID, domain.MemoryScopeEpisodic, "")
		if err != nil {
			return nil, "", mapRepositoryError(err)
		}
		allowed := make(map[string]struct{}, len(grantedSessions))
		for _, sessionID := range grantedSessions {
			allowed[sessionID] = struct{}{}
		}
		for _, memory := range items {
			if _, ok := allowed[memory.SourceSessionID]; ok {
				appendMemories([]domain.Memory{memory})
			}
		}
	}

	return collected, a.scopeNote(grant, len(grantedSessions)), nil
}

// scopeNote says what was searched. Without it an empty result reads as "this user has
// nothing", which is a different claim from "nothing authorized matched" - and the second
// is the only one this code is entitled to make.
func (a *MemoryAccess) scopeNote(grant MemoryGrant, grantedSessions int) string {
	parts := make([]string, 0, 3)
	if grant.AllowGlobal {
		parts = append(parts, "全局记忆")
	}
	if keys := normalizeKeys(grant.TopicKeys); len(keys) > 0 {
		parts = append(parts, "已授权主题的记忆（"+strings.Join(keys, "、")+"）")
	}
	if grantedSessions > 0 {
		parts = append(parts, "本轮授权会话里的经历记录")
	}
	if len(parts) == 0 {
		return "本轮没有记忆检索授权。"
	}
	note := "本轮检索范围：当前用户的" + strings.Join(parts, "、") + "；未列入的层与未授权的主题不参与检索。"
	if !grant.AllowGlobal || len(normalizeKeys(grant.TopicKeys)) == 0 {
		note += "没有找到内容不能说明该用户没有相关记忆，只能说授权范围内没有匹配。"
	}
	return note
}

// referenceFor builds the internal handle from the stored row. The citation id is derived
// here and nowhere else, so an answer can only cite a memory this read actually returned.
func referenceFor(memory domain.Memory) MemoryReference {
	title := "全局记忆"
	switch memory.Scope {
	case domain.MemoryScopeTopic:
		title = "主题记忆"
		if topic := strings.TrimSpace(memory.TopicKey); topic != "" {
			title += "：" + topic
		}
	case domain.MemoryScopeEpisodic:
		title = "经历记录"
	}
	return MemoryReference{
		MemoryID:         memory.MemoryID,
		Scope:            memory.Scope,
		TopicKey:         memory.TopicKey,
		SourceSessionID:  memory.SourceSessionID,
		SourceMessageIDs: append([]string(nil), memory.SourceMessageIDs...),
		Confidence:       memory.Confidence,
		Citation: domain.Source{
			SourceID: "src-" + memory.MemoryID,
			Title:    title,
			Snippet:  domain.Snippet(memory.Content, 280),
		},
	}
}

// overlap counts how many distinct query tokens the text contains, and how many distinct
// query tokens there were. Returning the pair keeps the score a ratio of covered query
// terms, which is comparable across candidates inside one call.
func overlap(queryTokens []string, content string) (matched int, total int) {
	needles := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		if token == "" {
			continue
		}
		needles[token] = struct{}{}
	}
	if len(needles) == 0 {
		return 0, 0
	}
	haystack := strings.ToLower(content)
	for token := range needles {
		if strings.Contains(haystack, token) {
			matched++
		}
	}
	return matched, len(needles)
}

func normalizeKeys(values []string) []string {
	keys := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
