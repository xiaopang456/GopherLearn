package repositorytest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// MemorySuite runs the long-term memory contract against one store. It exists because the
// confirmation gate is a security-relevant rule, not a storage detail: if one store lets a
// pending draft into a recallable list, or lets a second confirmation stand beside the
// first, then whether the system remembers nonsense depends on which repository the
// process was configured with.
//
// Every subtest builds its own store, so no case can inherit another's rows.
func MemorySuite(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := t.Context()

	t.Run("a draft is stored pending with its provenance in order", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		first, second := mustMessage(t, store, userID, sessionID, "first"), mustMessage(t, store, userID, sessionID, "second")

		stored := mustDraft(t, store, userID, sessionID, []string{second, first}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "  我用 Go 写后端  ", Confidence: 0.4,
		})
		if stored.Status != domain.MemoryStatusPending {
			t.Fatalf("status = %q, want pending: a draft must not be able to arrive confirmed", stored.Status)
		}
		if stored.MemoryID == "" || stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
			t.Fatalf("stored identity missing: %#v", stored)
		}
		if stored.Content != "我用 Go 写后端" {
			t.Fatalf("content = %q, want the surrounding space trimmed", stored.Content)
		}
		if got := stored.SourceMessageIDs; !reflect.DeepEqual(got, []string{second, first}) {
			t.Fatalf("provenance = %v, want the cited order preserved", got)
		}
		read, err := store.GetMemory(ctx, userID, stored.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(read.SourceMessageIDs, []string{second, first}) {
			t.Fatalf("re-read provenance = %v, want the same order", read.SourceMessageIDs)
		}
		if read.UserID != userID || read.SourceSessionID != sessionID || read.Confidence != 0.4 {
			t.Fatalf("re-read = %+v, want the draft as filed", read)
		}
		if read.Recallable() {
			t.Fatal("a pending draft reported itself recallable")
		}
	})

	t.Run("invalid drafts are refused before anything is written", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		other := mustUser(t, store, "other")
		session := mustSession(t, store, owner)
		otherSession := mustSession(t, store, other)
		message := mustMessage(t, store, owner, session, "mine")
		foreignMessage := mustMessage(t, store, other, otherSession, "theirs")

		cases := []struct {
			name   string
			userID string
			draft  domain.MemoryDraft
			want   error
		}{
			{"no user", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"unknown scope", "", domain.MemoryDraft{Scope: "profile", TopicKey: "go", Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"topic without a key", "", domain.MemoryDraft{Scope: domain.MemoryScopeTopic, Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"global carrying a topic key", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, TopicKey: "go", Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"blank content", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "   ", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"confidence above the range", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", Confidence: 1.5, SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"no source session", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceMessageIDs: []string{message}}, repository.ErrInvalidArgument},
			{"no source messages", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: session}, repository.ErrInvalidArgument},
			{"repeated source message", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message, message}}, repository.ErrInvalidArgument},
			// Somebody else's session and a session that never existed are the same answer,
			// everywhere in this repository: creating a draft cannot be used to probe.
			{"another user's session", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: otherSession, SourceMessageIDs: []string{foreignMessage}}, repository.ErrNotFound},
			{"a message from elsewhere", "", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: session, SourceMessageIDs: []string{foreignMessage}}, repository.ErrInvalidArgument},
			{"unknown owner", "user-who-does-not-exist", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "x", SourceSessionID: session, SourceMessageIDs: []string{message}}, repository.ErrNotFound},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				draft := testCase.draft
				draft.UserID = testCase.userID
				if draft.UserID == "" && testCase.name != "no user" {
					draft.UserID = owner
				}
				if _, err := store.CreateMemoryDraft(ctx, &draft); !errors.Is(err, testCase.want) {
					t.Fatalf("error = %v, want %v", err, testCase.want)
				}
			})
		}
	})

	t.Run("a draft is invisible to every scoped listing", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")

		for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic, domain.MemoryScopeEpisodic} {
			stored := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
				Scope: scope, TopicKey: topicKeyFor(scope), Content: "未确认的候选",
			})
			found, err := store.ListActiveMemories(ctx, userID, scope, topicKeyFor(scope))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 0 {
				t.Fatalf("%s listing = %+v, want nothing: an unconfirmed proposal is not a fact", scope, found)
			}
			if _, err := store.GetMemory(ctx, userID, stored.MemoryID); err != nil {
				t.Fatalf("the owner cannot see their own draft: %v", err)
			}
		}
	})

	t.Run("confirmation is what makes a memory recallable", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")

		for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic, domain.MemoryScopeEpisodic} {
			stored := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
				Scope: scope, TopicKey: topicKeyFor(scope), Content: "已确认",
			})
			confirmed, err := store.ConfirmMemoryDraft(ctx, userID, stored.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			if confirmed.Status != domain.MemoryStatusActive || !confirmed.Recallable() {
				t.Fatalf("confirmed = %+v, want active and recallable", confirmed)
			}
			if confirmed.SupersededAt.IsZero() == false {
				t.Fatalf("a live memory carries a superseded time: %+v", confirmed)
			}
			found, err := store.ListActiveMemories(ctx, userID, scope, topicKeyFor(scope))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].MemoryID != stored.MemoryID {
				t.Fatalf("%s listing = %+v, want the confirmed memory", scope, found)
			}
		}
	})

	t.Run("rejected and archived memories leave the recallable set and cannot return", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")

		rejected := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "被拒绝",
		})
		archived, err := store.RejectMemoryDraft(ctx, userID, rejected.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if archived.Status != domain.MemoryStatusArchived {
			t.Fatalf("reject left status %q", archived.Status)
		}
		assertNotRecallable(t, store, userID, rejected.MemoryID)

		// A rejected proposal cannot be confirmed later: the user said no, and that
		// answer is the reason the row is kept rather than deleted.
		if _, err := store.ConfirmMemoryDraft(ctx, userID, rejected.MemoryID); !isStateConflict(err, domain.MemoryStatusArchived) {
			t.Fatalf("confirming a rejected draft = %v, want an archived-state conflict", err)
		}

		live := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "已过期",
		})
		if _, err := store.ConfirmMemoryDraft(ctx, userID, live.MemoryID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ArchiveMemory(ctx, userID, live.MemoryID); err != nil {
			t.Fatal(err)
		}
		assertNotRecallable(t, store, userID, live.MemoryID)
		if _, err := store.ConfirmMemoryDraft(ctx, userID, live.MemoryID); !isStateConflict(err, domain.MemoryStatusArchived) {
			t.Fatalf("confirming an archived memory = %v, want an archived-state conflict", err)
		}
		if _, err := store.ArchiveMemory(ctx, userID, live.MemoryID); !isStateConflict(err, domain.MemoryStatusArchived) {
			t.Fatalf("archiving twice = %v, want an archived-state conflict", err)
		}
		// Only a draft can be rejected: rejecting something the user confirmed would
		// undo a decision through an operation named for a different one.
		fresh := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "还没确认",
		})
		if _, err := store.ArchiveMemory(ctx, userID, fresh.MemoryID); !isStateConflict(err, domain.MemoryStatusPending) {
			t.Fatalf("archiving a draft = %v, want a pending-state conflict", err)
		}
		if _, err := store.RejectMemoryDraft(ctx, userID, live.MemoryID); !isStateConflict(err, domain.MemoryStatusArchived) {
			t.Fatalf("rejecting an archived memory = %v, want an archived-state conflict", err)
		}
	})

	t.Run("confirming twice cannot take effect twice", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")
		stored := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "一次就够",
		})
		if _, err := store.ConfirmMemoryDraft(ctx, userID, stored.MemoryID); err != nil {
			t.Fatal(err)
		}
		second, err := store.ConfirmMemoryDraft(ctx, userID, stored.MemoryID)
		if !isStateConflict(err, domain.MemoryStatusActive) {
			t.Fatalf("second confirm = (%+v, %v), want an active-state conflict", second, err)
		}
		if !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("the state conflict does not match ErrConflict: %v", err)
		}
		found, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 1 || found[0].MemoryID != stored.MemoryID {
			t.Fatalf("global listing = %+v, want exactly the one active memory", found)
		}
	})

	t.Run("a new confirmation supersedes the older one for the same subject", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")

		for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic} {
			older := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
				Scope: scope, TopicKey: topicKeyFor(scope), Content: "旧答案",
			})
			newer := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
				Scope: scope, TopicKey: topicKeyFor(scope), Content: "新答案",
			})
			// Re-read rather than trust the value the first confirmation returned: that
			// copy was taken before the second confirmation moved it, and the whole
			// question is what the stored row says now.
			predecessor, err := store.GetMemory(ctx, userID, older.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			if predecessor.Status != domain.MemoryStatusSuperseded {
				t.Fatalf("%s: predecessor status = %q, want superseded", scope, predecessor.Status)
			}
			if predecessor.SupersededAt.IsZero() {
				t.Fatalf("%s: a superseded memory has no superseded time", scope)
			}
			found, err := store.ListActiveMemories(ctx, userID, scope, topicKeyFor(scope))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].MemoryID != newer.MemoryID {
				t.Fatalf("%s: listing = %+v, want only the newer active version", scope, found)
			}
			// A different topic is a different subject: its version history must not be
			// touched by this one.
			if scope == domain.MemoryScopeTopic {
				parallel, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeTopic, "另一个主题")
				if err != nil {
					t.Fatal(err)
				}
				if len(parallel) != 0 {
					t.Fatalf("an unrelated topic was caught up in the supersede: %+v", parallel)
				}
			}
		}

		// Episodes accumulate: two experienced events are two facts, and silently
		// retiring the first would rewrite what happened.
		firstEpisode := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeEpisodic, Content: "第一次决定",
		})
		secondEpisode := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeEpisodic, TopicKey: "部署", Content: "第二次决定",
		})
		keptEpisode, err := store.GetMemory(ctx, userID, firstEpisode.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if keptEpisode.Status != domain.MemoryStatusActive {
			t.Fatalf("the first episode ended as %q, want it kept active beside the second", keptEpisode.Status)
		}
		all, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeEpisodic, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 || all[0].MemoryID != firstEpisode.MemoryID || all[1].MemoryID != secondEpisode.MemoryID {
			t.Fatalf("episodes = %+v, want both in creation order", all)
		}
		keyed, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeEpisodic, "部署")
		if err != nil {
			t.Fatal(err)
		}
		if len(keyed) != 1 || keyed[0].MemoryID != secondEpisode.MemoryID {
			t.Fatalf("episodes for one topic = %+v, want only the keyed one", keyed)
		}
	})

	t.Run("a hand-written memory is the decision and cites no conversation", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		// A real session and a real message exist here on purpose. The authored write must
		// not reach for them: if a record could pick up provenance that happened to be
		// lying around, the difference between "I said this about myself" and "the agent
		// claims the conversation showed this" would disappear into the row.
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")

		for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic} {
			stored := mustAuthored(t, store, userID, domain.UserAuthoredMemory{
				Scope: scope, TopicKey: topicKeyFor(scope), Content: "  我偏好先看代码再读解释  ",
			})
			if stored.Status != domain.MemoryStatusActive {
				t.Fatalf("%s: status = %q, want active - submitting it IS the confirmation", scope, stored.Status)
			}
			if stored.Origin != domain.MemoryOriginUserAuthored {
				t.Fatalf("%s: origin = %q, want %q", scope, stored.Origin, domain.MemoryOriginUserAuthored)
			}
			if stored.Confidence != 1 {
				t.Fatalf("%s: confidence = %v, want the server's 1 for a statement nobody graded", scope, stored.Confidence)
			}
			if stored.Content != "我偏好先看代码再读解释" {
				t.Fatalf("%s: content = %q, want the surrounding space trimmed", scope, stored.Content)
			}
			if stored.SourceSessionID != "" || len(stored.SourceMessageIDs) != 0 {
				t.Fatalf("%s: an authored write grew provenance: %+v", scope, stored)
			}
			read, err := store.GetMemory(ctx, userID, stored.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			if read.SourceSessionID != "" || len(read.SourceMessageIDs) != 0 {
				t.Fatalf("%s: re-read grew a source nobody cited: %+v", scope, read)
			}
			if !read.Recallable() {
				t.Fatalf("%s: an active memory reported itself unrecallable", scope)
			}
			// The row satisfies its own origin rule: an authored record with no
			// provenance is valid, and the same shape carrying a source would not be.
			if err := read.Validate(); err != nil {
				t.Fatalf("%s: the stored row does not validate: %v", scope, err)
			}
			if read.AuthoredBy() != domain.MemoryOriginUserAuthored {
				t.Fatalf("%s: the stored row classifies itself as %q", scope, read.AuthoredBy())
			}
			found, err := store.ListActiveMemories(ctx, userID, scope, topicKeyFor(scope))
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].MemoryID != stored.MemoryID {
				t.Fatalf("%s: active listing = %+v, want the one hand-written memory", scope, found)
			}
			// And it is nowhere near the confirmation queue: a row that still needed
			// deciding would be a second gate on a decision already made.
			waiting, err := store.ListMemoriesByStatus(ctx, userID, domain.MemoryStatusPending)
			if err != nil {
				t.Fatal(err)
			}
			if len(waiting) != 0 {
				t.Fatalf("%s: pending listing = %+v, want nothing waiting after a hand-written add", scope, waiting)
			}
		}

		// The other path is untouched by the existence of this one: a proposal still
		// arrives pending, still classifies as agent-proposed, and still needs evidence.
		proposed := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "代理从会话里读到的一条",
		})
		if proposed.Status != domain.MemoryStatusPending || proposed.Origin != domain.MemoryOriginAgentProposed {
			t.Fatalf("proposal = %+v, want pending and agent-proposed", proposed)
		}
	})

	t.Run("a hand-written memory that names what it may not is refused and writes nothing", func(t *testing.T) {
		cases := []struct {
			name   string
			author string // "owner", "" for nobody, "unknown" for an account the store does not have
			want   error
			shape  domain.UserAuthoredMemory
		}{
			{
				name: "an episode, which by definition happened in a conversation", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeEpisodic, Content: "上次我们决定用 everysec"},
			},
			{
				name: "an episode with a subject key", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeEpisodic, TopicKey: "redis", Content: "一句话"},
			},
			{
				name: "a global fact carrying a topic key", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeGlobal, TopicKey: "go", Content: "一句话"},
			},
			{
				name: "a topic fact with no subject to be about", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeTopic, Content: "一句话"},
			},
			{
				name: "a scope this server does not have", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: "profile", Content: "一句话"},
			},
			{
				name: "no content", author: "owner",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeGlobal, Content: "   "},
			},
			{
				name: "content past the stored limit", author: "owner",
				want: repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{
					Scope: domain.MemoryScopeGlobal, Content: strings.Repeat("长", domain.MaxMemoryContentRunes+1),
				},
			},
			{
				name: "no author", author: "",
				want:  repository.ErrInvalidArgument,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeGlobal, Content: "一句话"},
			},
			{
				// An account that does not exist and one the caller is not are the same
				// answer as everywhere else in this store: adding a memory cannot probe.
				name: "an author the store does not have", author: "unknown",
				want:  repository.ErrNotFound,
				shape: domain.UserAuthoredMemory{Scope: domain.MemoryScopeGlobal, Content: "一句话"},
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				// One store per case. Sharing one would make the second case's "nothing was
				// written" assertion depend on the first case's outcome, and a single wrongly
				// accepted write would then look like nine failures instead of one.
				store := newStore(t)
				owner := mustUser(t, store, "owner")
				other := mustUser(t, store, "other")
				author := owner
				switch testCase.author {
				case "":
					author = ""
				case "unknown":
					author = "user-who-does-not-exist"
				}

				authored := testCase.shape
				authored.UserID = author
				if _, err := store.StoreUserAuthoredMemory(ctx, &authored); !errors.Is(err, testCase.want) {
					t.Fatalf("error = %v, want %v", err, testCase.want)
				}
				// Refusal means refusal: no row for the would-be author, and none for the
				// account that does exist either.
				for _, userID := range []string{owner, other} {
					for _, status := range []domain.MemoryStatus{domain.MemoryStatusActive, domain.MemoryStatusPending} {
						rows, err := store.ListMemoriesByStatus(ctx, userID, status)
						if err != nil {
							t.Fatal(err)
						}
						if len(rows) != 0 {
							t.Fatalf("a refused write left %d %s row(s) for %s: %+v", len(rows), status, userID, rows)
						}
					}
				}
			})
		}
	})

	t.Run("a hand-written memory retires the active answer to its own subject and nothing else", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		neighbour := mustUser(t, store, "neighbour")
		sessionID := mustSession(t, store, userID)
		neighbourSession := mustSession(t, store, neighbour)
		message := mustMessage(t, store, userID, sessionID, "evidence")
		neighbourMessage := mustMessage(t, store, neighbour, neighbourSession, "theirs")

		// The predecessor is an *agent-confirmed* active row: one subject may hold one
		// active answer regardless of which path produced the previous one, or the recall
		// read would have two of them to choose between.
		olderGlobal := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "旧的全球事实",
		})
		olderTopic := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "旧的 redis 事实",
		})
		keptTopic := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeTopic, TopicKey: "kafka", Content: "另一个主题",
		})
		foreign := mustConfirm(t, store, neighbour, neighbourSession, []string{neighbourMessage}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "别人的事实",
		})
		// A proposal about the same subject is not an active answer, so replacing the
		// answer must leave the decision itself untouched.
		waiting := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "还在等的候选",
		})

		newGlobal := mustAuthored(t, store, userID, domain.UserAuthoredMemory{
			Scope: domain.MemoryScopeGlobal, Content: "我写的全球事实",
		})
		newTopic := mustAuthored(t, store, userID, domain.UserAuthoredMemory{
			Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "我写的 redis 事实",
		})

		for name, retired := range map[string]*domain.Memory{
			"the global predecessor": olderGlobal, "the topic predecessor": olderTopic,
		} {
			read, err := store.GetMemory(ctx, userID, retired.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			if read.Status != domain.MemoryStatusSuperseded {
				t.Fatalf("%s ended as %q, want superseded by the hand-written replacement", name, read.Status)
			}
			if read.SupersededAt.IsZero() {
				t.Fatalf("%s has no superseded time: %+v", name, read)
			}
			// A retired row stays a valid record: the status moved, the shape did not.
			if err := read.Validate(); err != nil {
				t.Fatalf("%s no longer validates: %v", name, err)
			}
		}
		for _, pair := range []struct {
			scope     domain.MemoryScope
			topicKey  string
			wantID    string
			wantLabel string
		}{
			{domain.MemoryScopeGlobal, "", newGlobal.MemoryID, "global"},
			{domain.MemoryScopeTopic, "redis", newTopic.MemoryID, "redis"},
		} {
			found, err := store.ListActiveMemories(ctx, userID, pair.scope, pair.topicKey)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].MemoryID != pair.wantID {
				t.Fatalf("%s: active listing = %+v, want exactly the new hand-written row", pair.wantLabel, found)
			}
		}
		// Untouched: a different subject, a different account, and an undecided proposal.
		kept, err := store.GetMemory(ctx, userID, keptTopic.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if kept.Status != domain.MemoryStatusActive {
			t.Fatalf("an unrelated topic ended as %q, want it untouched", kept.Status)
		}
		foreignRead, err := store.GetMemory(ctx, neighbour, foreign.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if foreignRead.Status != domain.MemoryStatusActive {
			t.Fatalf("another account's memory was caught up in this write: %q", foreignRead.Status)
		}
		stillWaiting, err := store.GetMemory(ctx, userID, waiting.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if stillWaiting.Status != domain.MemoryStatusPending {
			t.Fatalf("the pending candidate came out %q, want it still waiting on its own decision", stillWaiting.Status)
		}
	})

	t.Run("another user can neither read nor change a memory", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		sessionID := mustSession(t, store, owner)
		message := mustMessage(t, store, owner, sessionID, "evidence")
		stored := mustDraft(t, store, owner, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "属于别人",
		})

		if _, err := store.GetMemory(ctx, intruder, stored.MemoryID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("read error = %v, want not found", err)
		}
		for name, call := range map[string]func() error{
			"confirm": func() error { _, err := store.ConfirmMemoryDraft(ctx, intruder, stored.MemoryID); return err },
			"reject":  func() error { _, err := store.RejectMemoryDraft(ctx, intruder, stored.MemoryID); return err },
			"archive": func() error { _, err := store.ArchiveMemory(ctx, intruder, stored.MemoryID); return err },
			"delete":  func() error { return store.DeleteMemory(ctx, intruder, stored.MemoryID) },
		} {
			if err := call(); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("%s by a stranger = %v, want not found", name, err)
			}
		}
		// The stranger's own listing cannot surface it either.
		found, err := store.ListActiveMemories(ctx, intruder, domain.MemoryScopeGlobal, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 0 {
			t.Fatalf("stranger listing = %+v, want empty", found)
		}
		if _, err := store.ConfirmMemoryDraft(ctx, owner, stored.MemoryID); err != nil {
			t.Fatalf("the owner's own confirmation was disturbed: %v", err)
		}
	})

	t.Run("deleting a session keeps memories and their recorded references", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		first, second := mustMessage(t, store, userID, sessionID, "first"), mustMessage(t, store, userID, sessionID, "second")

		// Two memories cite the same conversation. A session delete that cascaded to
		// memories would destroy confirmed facts on the strength of housekeeping, and it
		// would do so for every memory that merely pointed at the conversation.
		confirmed := mustConfirm(t, store, userID, sessionID, []string{first}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "已确认的事实",
		})
		pending := mustDraft(t, store, userID, sessionID, []string{second}, domain.MemoryDraft{
			Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "还没确认",
		})
		if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
			t.Fatal(err)
		}

		live, err := store.GetMemory(ctx, userID, confirmed.MemoryID)
		if err != nil {
			t.Fatalf("the confirmed memory vanished with its session: %v", err)
		}
		if live.Status != domain.MemoryStatusActive {
			t.Fatalf("status after the delete = %q", live.Status)
		}
		if !reflect.DeepEqual(live.SourceMessageIDs, []string{first}) {
			t.Fatalf("the reference was rewritten: %v", live.SourceMessageIDs)
		}
		if _, err := store.GetMemory(ctx, userID, pending.MemoryID); err != nil {
			t.Fatalf("the draft vanished with its session: %v", err)
		}
		// What is gone is the evidence, so the draft that cites it can no longer become a
		// fact. Refusing is the only honest answer: confirming it would create a memory
		// nobody can check any more.
		if _, err := store.ConfirmMemoryDraft(ctx, userID, pending.MemoryID); !errors.Is(err, repository.ErrMemoryProvenanceMissing) {
			t.Fatalf("confirming after the evidence was deleted = %v, want the provenance error", err)
		}
		if current, err := store.GetMemory(ctx, userID, pending.MemoryID); err != nil || current.Status != domain.MemoryStatusPending {
			t.Fatalf("the refused draft changed anyway: %+v err %v", current, err)
		}
	})

	t.Run("listing requires the key rule the records follow", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		if _, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "redis"); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("a global listing narrowed by topic = %v, want invalid", err)
		}
		if _, err := store.ListActiveMemories(ctx, userID, domain.MemoryScopeTopic, ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("a topic listing without a key = %v, want invalid", err)
		}
		if _, err := store.ListActiveMemories(ctx, userID, domain.MemoryScope("mood"), ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("an unknown scope = %v, want invalid", err)
		}
		if _, err := store.ListActiveMemories(ctx, "", domain.MemoryScopeGlobal, ""); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("an empty user = %v, want invalid", err)
		}
	})

	t.Run("an idempotent draft write answers with the row already waiting", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "第一条证据")
		second := mustMessage(t, store, userID, sessionID, "第二条证据")
		otherSession := mustSession(t, store, intruder)

		claim := func(content string, keys ...string) *domain.MemoryDraft {
			return &domain.MemoryDraft{
				UserID: userID, Scope: domain.MemoryScopeGlobal, Content: content, Confidence: 1,
				SourceSessionID: sessionID, SourceMessageIDs: keys,
			}
		}

		first, created, err := store.CreateMemoryDraftOnce(ctx, claim("先看代码再读文档", message))
		if err != nil || !created {
			t.Fatalf("first proposal = created %v err %v, want a new row", created, err)
		}
		if first.Status != domain.MemoryStatusPending {
			t.Fatalf("an idempotent write stored a %q row, want pending", first.Status)
		}
		again, created, err := store.CreateMemoryDraftOnce(ctx, claim("先看代码再读文档", message))
		if err != nil || created {
			t.Fatalf("repeat of the same claim = created %v err %v, want the existing row", created, err)
		}
		if again.MemoryID != first.MemoryID {
			t.Fatalf("repeat returned %q, want the row already waiting %q", again.MemoryID, first.MemoryID)
		}
		if pending := mustList(t, store, userID, domain.MemoryStatusPending); len(pending) != 1 {
			t.Fatalf("the same claim produced %d pending rows, want one: %+v", len(pending), pending)
		}

		// Anything the identity of a claim depends on must break the match, or a genuine
		// second thought gets swallowed by the first one.
		nearMisses := []*domain.MemoryDraft{
			claim("先看代码再读文档吧", message),        // different words
			claim("先看代码再读文档", second),          // different evidence
			claim("先看代码再读文档", message, second), // more evidence
			claim("先看代码再读文档", second, message), // same evidence, other citation order
		}
		for index, draft := range nearMisses {
			if _, wasCreated, err := store.CreateMemoryDraftOnce(ctx, draft); err != nil || !wasCreated {
				t.Fatalf("near-miss %d = created %v err %v, want a new row", index, wasCreated, err)
			}
		}
		// A topic memory under another key is another subject, not a duplicate.
		topic := func(key string) *domain.MemoryDraft {
			return &domain.MemoryDraft{UserID: userID, Scope: domain.MemoryScopeTopic, TopicKey: key,
				Content: "AOF 选 everysec", Confidence: 1, SourceSessionID: sessionID, SourceMessageIDs: []string{message}}
		}
		if _, wasCreated, err := store.CreateMemoryDraftOnce(ctx, topic("redis")); err != nil || !wasCreated {
			t.Fatalf("first topic proposal = created %v err %v, want a new row", wasCreated, err)
		}
		if _, wasCreated, err := store.CreateMemoryDraftOnce(ctx, topic("mysql")); err != nil || !wasCreated {
			t.Fatalf("topic proposal under another key = created %v err %v, want a new row", wasCreated, err)
		}
		if _, wasCreated, err := store.CreateMemoryDraftOnce(ctx, topic("redis")); err != nil || wasCreated {
			t.Fatalf("repeat under the same topic key = created %v err %v, want the existing row", wasCreated, err)
		}

		// Confirming retires the claim from the pending set, so proposing it again is a new
		// decision for the user rather than a duplicate of something still waiting.
		if _, err := store.ConfirmMemoryDraft(ctx, userID, first.MemoryID); err != nil {
			t.Fatal(err)
		}
		if _, wasCreated, err := store.CreateMemoryDraftOnce(ctx, claim("先看代码再读文档", message)); err != nil || !wasCreated {
			t.Fatalf("proposal after the original was confirmed = created %v err %v, want a new pending row", wasCreated, err)
		}

		// The person-facing write stays literally what its name says: no deduping, because
		// someone typing the same sentence twice meant to send it twice.
		plain, err := store.CreateMemoryDraft(ctx, claim("手写的重复条款", message))
		if err != nil {
			t.Fatal(err)
		}
		twin, err := store.CreateMemoryDraft(ctx, claim("手写的重复条款", message))
		if err != nil {
			t.Fatal(err)
		}
		if plain.MemoryID == twin.MemoryID {
			t.Fatal("CreateMemoryDraft deduped, which is the Once variant's contract to keep")
		}

		// The refusals are the same refusals, so an idempotent path is not a looser one.
		bad := &domain.MemoryDraft{UserID: userID, Scope: domain.MemoryScopeTopic, Content: "没有主题键",
			Confidence: 1, SourceSessionID: sessionID, SourceMessageIDs: []string{message}}
		if _, _, err := store.CreateMemoryDraftOnce(ctx, bad); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("a topic draft with no key = %v, want invalid", err)
		}
		stealing := &domain.MemoryDraft{UserID: userID, Scope: domain.MemoryScopeGlobal, Content: "别人的会话",
			Confidence: 1, SourceSessionID: otherSession, SourceMessageIDs: []string{message}}
		if _, _, err := store.CreateMemoryDraftOnce(ctx, stealing); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a draft citing another account's session = %v, want not found", err)
		}
	})

	t.Run("the status listing is the owner's whole view of one state", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")
		otherSession := mustSession(t, store, intruder)
		otherMessage := mustMessage(t, store, intruder, otherSession, "their evidence")

		// Three scopes plus a superseded and an archived row: the point of this read is
		// that a topic memory is enumerable by its owner without knowing its key first,
		// which is what ListActiveMemories deliberately refuses.
		pendingGlobal := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "第一条待确认",
		})
		pendingTopic := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "主题待确认",
		})
		foreignDraft := mustDraft(t, store, intruder, otherSession, []string{otherMessage}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "别人的待确认",
		})
		pending, err := store.ListMemoriesByStatus(ctx, userID, domain.MemoryStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		if got := memoryIDs(pending); len(got) != 2 || got[0] != pendingGlobal.MemoryID || got[1] != pendingTopic.MemoryID {
			t.Fatalf("pending listing = %v, want this user's two drafts in creation order", got)
		}
		if ids := memoryIDs(mustList(t, store, intruder, domain.MemoryStatusPending)); len(ids) != 1 || ids[0] != foreignDraft.MemoryID {
			t.Fatalf("the other account's listing = %v, want only its own draft", ids)
		}

		confirmed := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeEpisodic, Content: "一次确认过的经历",
		})
		// Two global confirmations in a row: the first is the one the second replaces, so
		// the superseded state has a real occupant to list.
		mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "被取代的全球事实",
		})
		replacement := mustConfirm(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "取代它的全球事实",
		})
		if ids := memoryIDs(mustList(t, store, userID, domain.MemoryStatusActive)); len(ids) != 2 || ids[0] != confirmed.MemoryID || ids[1] != replacement.MemoryID {
			t.Fatalf("active listing = %v, want the episode then the new global fact", ids)
		}
		// Superseded and archived stay reachable by their own status, so "what happened to
		// my old answer" stays answerable without the row being recallable anywhere.
		superseded := memoryIDs(mustList(t, store, userID, domain.MemoryStatusSuperseded))
		if len(superseded) != 1 || superseded[0] == replacement.MemoryID {
			t.Fatalf("superseded listing = %v, want exactly one row and not the current one", superseded)
		}
		rejected := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeEpisodic, Content: "会被拒绝的一条",
		})
		if _, err := store.RejectMemoryDraft(ctx, userID, rejected.MemoryID); err != nil {
			t.Fatal(err)
		}
		if ids := memoryIDs(mustList(t, store, userID, domain.MemoryStatusArchived)); len(ids) != 1 || ids[0] != rejected.MemoryID {
			t.Fatalf("archived listing = %v, want the rejected draft", ids)
		}
		if _, err := store.ListMemoriesByStatus(ctx, userID, domain.MemoryStatus("decided")); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("an unknown status = %v, want invalid", err)
		}
		if _, err := store.ListMemoriesByStatus(ctx, "", domain.MemoryStatusPending); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("an empty user = %v, want invalid", err)
		}
	})

	t.Run("a deleted memory is gone and cannot be revived through another call", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")
		stored := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeGlobal, Content: "删掉我",
		})
		if err := store.DeleteMemory(ctx, userID, stored.MemoryID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetMemory(ctx, userID, stored.MemoryID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("read after delete = %v, want not found", err)
		}
		if _, err := store.ConfirmMemoryDraft(ctx, userID, stored.MemoryID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("confirm after delete = %v, want not found", err)
		}
		if err := store.DeleteMemory(ctx, userID, stored.MemoryID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("second delete = %v, want not found", err)
		}
	})

	t.Run("returned memories are copies", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "owner")
		sessionID := mustSession(t, store, userID)
		message := mustMessage(t, store, userID, sessionID, "evidence")
		stored := mustDraft(t, store, userID, sessionID, []string{message}, domain.MemoryDraft{
			Scope: domain.MemoryScopeEpisodic, Content: "一次经历",
		})
		stored.SourceMessageIDs[0] = "rewritten"
		stored.Content = "rewritten"

		read, err := store.GetMemory(ctx, userID, stored.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if read.Content != "一次经历" || !reflect.DeepEqual(read.SourceMessageIDs, []string{message}) {
			t.Fatalf("a caller's edit reached the store: %+v", read)
		}
	})
}

// mustList reads the owner's view of one state and fails the test on any error, so the
// listing assertions below read as one line each.
func mustList(t *testing.T, store repository.Repository, userID string, status domain.MemoryStatus) []domain.Memory {
	t.Helper()
	items, err := store.ListMemoriesByStatus(t.Context(), userID, status)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func memoryIDs(items []domain.Memory) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.MemoryID)
	}
	return ids
}

func mustMessage(t *testing.T, store repository.Repository, userID, sessionID, content string) string {
	t.Helper()
	message, err := store.AppendMessage(t.Context(), userID, sessionID, &domain.Message{Role: "user", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return message.ID
}

func mustDraft(t *testing.T, store repository.Repository, userID, sessionID string, sources []string, shape domain.MemoryDraft) *domain.Memory {
	t.Helper()
	draft := shape
	draft.UserID = userID
	draft.SourceSessionID = sessionID
	draft.SourceMessageIDs = append([]string(nil), sources...)
	if draft.Content == "" {
		draft.Content = "一条记忆"
	}
	stored, err := store.CreateMemoryDraft(t.Context(), &draft)
	if err != nil {
		t.Fatalf("create draft (%s): %v", draft.Scope, err)
	}
	return stored
}

func mustConfirm(t *testing.T, store repository.Repository, userID, sessionID string, sources []string, shape domain.MemoryDraft) *domain.Memory {
	t.Helper()
	stored := mustDraft(t, store, userID, sessionID, sources, shape)
	confirmed, err := store.ConfirmMemoryDraft(t.Context(), userID, stored.MemoryID)
	if err != nil {
		t.Fatalf("confirm draft: %v", err)
	}
	return confirmed
}

// mustAuthored stores one hand-written memory and fails the test if the store refused it,
// which is how the cases above read as a contract rather than as a wish: a store that
// cannot take what a person typed has failed here, not somewhere the test does not look.
func mustAuthored(
	t *testing.T, store repository.Repository, userID string, shape domain.UserAuthoredMemory,
) *domain.Memory {
	t.Helper()
	authored := shape
	authored.UserID = userID
	if authored.Content == "" {
		authored.Content = "一条手写的记忆"
	}
	stored, err := store.StoreUserAuthoredMemory(t.Context(), &authored)
	if err != nil {
		t.Fatalf("store user-authored memory (%s): %v", authored.Scope, err)
	}
	return stored
}

// topicKeyFor gives each scope the key shape it requires: a global memory must not name a
// topic, a topic memory must, and an episode may go either way.
func topicKeyFor(scope domain.MemoryScope) string {
	if scope == domain.MemoryScopeTopic {
		return "redis"
	}
	return ""
}

func assertNotRecallable(t *testing.T, store repository.Repository, userID, memoryID string) {
	t.Helper()
	for _, scope := range []domain.MemoryScope{domain.MemoryScopeGlobal, domain.MemoryScopeTopic, domain.MemoryScopeEpisodic} {
		found, err := store.ListActiveMemories(t.Context(), userID, scope, topicKeyFor(scope))
		if err != nil {
			t.Fatal(err)
		}
		for _, memory := range found {
			if memory.MemoryID == memoryID {
				t.Fatalf("%s listing still returns %s, want it gone", scope, memoryID)
			}
		}
	}
}

func isStateConflict(err error, found domain.MemoryStatus) bool {
	var conflict *repository.MemoryStateConflict
	return errors.As(err, &conflict) && conflict.Status == found
}
