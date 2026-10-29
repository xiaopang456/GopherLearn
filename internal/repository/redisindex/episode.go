package redisindex

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	redis "github.com/redis/go-redis/v9"
)

// The episode retrieval view. It has its own index and key prefix because a
// session projection has a different lifecycle and different ranking fields
// from knowledge chunks - one derived card per session, rewritten on every
// re-projection, deleted when the session goes - and because mixing the two in
// one index would let a query drift into the other's fields.
//
// MySQL stays authoritative. Every candidate this index proposes is re-read
// through the store (owner, active session, active status, indexed flag) before
// it can become evidence, so a stale or half-written key here costs recall, not
// correctness. The same rule is what makes the delete path best-effort: a key
// that survives a crashed delete is filtered by the reload, not trusted.

const (
	// DefaultEpisodeIndexName is one versioned index per schema.
	DefaultEpisodeIndexName = "gopherlearn_memory_episodes_v1_idx"
	// DefaultEpisodeKeyPrefix scopes every hashed episode under one prefix.
	// key = prefix + userID + ":" + episodeID
	DefaultEpisodeKeyPrefix = "gopherlearn:memory:episode:"
)

// Episode field names, exported so a test and an operator can name the schema.
const (
	EpisodeFieldEpisodeID     = "episode_id"
	EpisodeFieldUserID        = "user_id"
	EpisodeFieldSessionID     = "session_id"
	EpisodeFieldStatus        = "status"
	EpisodeFieldLastMessageAt = "last_message_at"
	EpisodeFieldTitle         = "title"
	EpisodeFieldTopicKeys     = "topic_keys"
	EpisodeFieldSearchText    = "search_text"
	EpisodeFieldSearchBigrams = "search_bigrams"
	EpisodeFieldIdentity      = "identity"
	EpisodeFieldVector        = "vector"
	episodeFieldDistance      = "distance"
	episodeMaxIndexedBigrams  = 600
	episodeScanChunk          = 500
)

// EpisodeDocument is the minimal projection of a stored episode row that the
// index needs. It is a struct rather than the domain type so this package does
// not import the domain: the fields are exactly what gets hashed.
type EpisodeDocument struct {
	EpisodeID     string
	UserID        string
	SessionID     string
	Status        string
	LastMessageAt int64 // unix seconds, sortable
	Title         string
	TopicKeys     []string
	SearchText    string
	Identity      string
	Vector        []float32
}

// EpisodeIndex is the RediSearch-backed episode retrieval view.
type EpisodeIndex struct {
	client       *redis.Client
	indexName    string
	keyPrefix    string
	dimensions   int
	operationTTL time.Duration
}

// EpisodeSearchResult is one hit from any of the three channels. The caller
// orders candidates itself; Rank is the position inside the channel that
// produced it.
type EpisodeSearchResult struct {
	EpisodeID     string
	LastMessageAt int64
	Score         float64
	Distance      float64
	Rank          int
}

// NewEpisodeIndex builds the episode index client. Like the knowledge index it
// performs no network I/O: connection failures surface from the first command.
func NewEpisodeIndex(options Options) (*EpisodeIndex, error) {
	host, password, database, err := resolveConnection(options.Address, options.Password, options.Database)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if options.Dimensions <= 0 {
		return nil, fmt.Errorf("%w: dimensions must be positive", ErrConfig)
	}
	name := strings.TrimSpace(options.IndexName)
	if name == "" {
		name = DefaultEpisodeIndexName
	}
	prefix := strings.TrimSpace(options.KeyPrefix)
	if prefix == "" {
		prefix = DefaultEpisodeKeyPrefix
	}
	ttl := options.OperationTTL
	if ttl <= 0 {
		ttl = DefaultOperationTimeout
	}
	return &EpisodeIndex{
		client: redis.NewClient(&redis.Options{
			Addr: host, Password: password, DB: database, Protocol: 2,
		}),
		indexName: name, keyPrefix: prefix, dimensions: options.Dimensions,
		operationTTL: ttl,
	}, nil
}

// Dimensions reports the width this index was built for.
func (e *EpisodeIndex) Dimensions() int { return e.dimensions }

// Name reports the index this client queries.
func (e *EpisodeIndex) Name() string { return e.indexName }

// Key builds the hash key for one episode.
func (e *EpisodeIndex) Key(userID, episodeID string) string {
	return e.keyPrefix + userID + ":" + episodeID
}

func (e *EpisodeIndex) contextWithTTL(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, set := ctx.Deadline(); set {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.operationTTL)
}

// EnsureIndex creates the index when absent and verifies an existing one field
// by field. Tolerating a drifted schema would let a search run against fields
// the rows never carried, so any drift refuses at startup instead.
func (e *EpisodeIndex) EnsureIndex(ctx context.Context) error {
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	if _, err := e.client.FTInfo(call, e.indexName).Result(); err != nil {
		if !isUnknownIndexError(err) {
			return describe(call, err)
		}
		if err := e.createIndex(call); err != nil {
			return err
		}
	}
	raw, rawErr := e.client.Do(call, "FT.INFO", e.indexName).Result()
	if rawErr != nil {
		return fmt.Errorf("inspect episode index raw: %w", rawErr)
	}
	flat, ok := raw.([]any)
	if !ok || len(flat)%2 != 0 {
		return fmt.Errorf("%w: unreadable FT.INFO for %s", ErrSchema, e.indexName)
	}
	if err := verifyEpisodeSchema(e.indexName, e.keyPrefix, e.dimensions, episodeInfoMap(flat)); err != nil {
		return err
	}
	return e.probeQuery(call)
}

func (e *EpisodeIndex) createIndex(call context.Context) error {
	schema := []*redis.FieldSchema{
		{FieldName: EpisodeFieldEpisodeID, FieldType: redis.SearchFieldTypeTag},
		{FieldName: EpisodeFieldUserID, FieldType: redis.SearchFieldTypeTag},
		{FieldName: EpisodeFieldSessionID, FieldType: redis.SearchFieldTypeTag},
		{FieldName: EpisodeFieldStatus, FieldType: redis.SearchFieldTypeTag},
		{FieldName: EpisodeFieldLastMessageAt, FieldType: redis.SearchFieldTypeNumeric, Sortable: true},
		{FieldName: EpisodeFieldTitle, FieldType: redis.SearchFieldTypeText, Weight: 3, NoStem: true},
		{FieldName: EpisodeFieldTopicKeys, FieldType: redis.SearchFieldTypeTag},
		{FieldName: EpisodeFieldSearchText, FieldType: redis.SearchFieldTypeText, Weight: 1, NoStem: true},
		// TAG, not TEXT: the index tokenizes Chinese with a dictionary that
		// splits these pairs straight back into single characters, which is the
		// failure they exist to work around. A tag value is stored verbatim, and
		// tag matches still contribute to BM25 scoring, so this stays one ranked
		// query rather than a second retrieval channel.
		{FieldName: EpisodeFieldSearchBigrams, FieldType: redis.SearchFieldTypeTag, Separator: ","},
		{FieldName: EpisodeFieldIdentity, FieldType: redis.SearchFieldTypeTag},
		{
			FieldName: EpisodeFieldVector, FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{FlatOptions: &redis.FTFlatOptions{
				Type: "FLOAT32", Dim: e.dimensions, DistanceMetric: "COSINE",
			}},
		},
	}
	options := &redis.FTCreateOptions{
		OnHash:          true,
		Prefix:          []interface{}{e.keyPrefix},
		DefaultLanguage: "chinese",
	}
	if err := e.client.FTCreate(call, e.indexName, options, schema...).Err(); err != nil {
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return describe(call, err)
	}
	return nil
}

// probeQuery asks the index the one question the whole view depends on: a KNN
// clause at the configured width. An index created by a build with a different
// dimension accepts writes of its own width and rejects this process's queries
// several steps away from the configuration that caused it; running the probe
// here turns that into a startup failure.
func (e *EpisodeIndex) probeQuery(call context.Context) error {
	probe := make([]float32, e.dimensions)
	probe[0] = 1
	statement := fmt.Sprintf("(@%s:{probetag})=>[KNN 1 @%s $probe_vector AS %s]",
		EpisodeFieldUserID, EpisodeFieldVector, episodeFieldDistance)
	if err := e.client.FTSearchWithArgs(call, e.indexName, statement, &redis.FTSearchOptions{
		Limit: 1, DialectVersion: 2,
		Params: map[string]interface{}{"probe_vector": vectorBytes(probe)},
	}).Err(); err != nil {
		return fmt.Errorf("%w: episode index %q does not answer a %d-dimensional query: %v",
			ErrConfig, e.indexName, e.dimensions, describe(call, err))
	}
	return nil
}

// Store writes the derived view for one card. The full field set is rewritten
// every time rather than merged, so a re-projection cannot leave a stale
// attribute beside a fresh vector.
func (e *EpisodeIndex) Store(ctx context.Context, document EpisodeDocument) error {
	if document.EpisodeID == "" || document.UserID == "" || len(document.Vector) != e.dimensions {
		return fmt.Errorf("%w: invalid episode index document", ErrConfig)
	}
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	if err := checkScopeValue("episode owner", document.UserID); err != nil {
		return err
	}
	if err := checkScopeValue("episode id", document.EpisodeID); err != nil {
		return err
	}
	identity := document.Identity
	if identity == "" {
		identity = unknownIdentity
	}
	if err := checkScopeValue("episode vector identity", identity); err != nil {
		return err
	}
	fields := map[string]interface{}{
		EpisodeFieldEpisodeID:     document.EpisodeID,
		EpisodeFieldUserID:        document.UserID,
		EpisodeFieldSessionID:     document.SessionID,
		EpisodeFieldStatus:        document.Status,
		EpisodeFieldLastMessageAt: document.LastMessageAt,
		EpisodeFieldTitle:         document.Title,
		EpisodeFieldTopicKeys:     strings.Join(document.TopicKeys, ","),
		EpisodeFieldSearchText:    document.SearchText,
		EpisodeFieldSearchBigrams: episodeBigramTagValue(document.Title + " " + document.SearchText),
		EpisodeFieldIdentity:      identity,
		EpisodeFieldVector:        vectorBytes(document.Vector),
	}
	if err := e.client.HSet(call, e.Key(document.UserID, document.EpisodeID), fields).Err(); err != nil {
		return describe(call, err)
	}
	return nil
}

// Delete drops the derived view for the named episodes. A failed delete is not
// a correctness problem - the reload filters stale rows - but it is still
// reported, because the key is storage the next reindex will have to clean.
func (e *EpisodeIndex) Delete(ctx context.Context, userID string, episodeIDs []string) error {
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	if err := checkScopeValue("episode owner", userID); err != nil {
		return err
	}
	keys := make([]string, 0, len(episodeIDs))
	for _, id := range episodeIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := checkScopeValue("episode id", id); err != nil {
			return err
		}
		keys = append(keys, e.Key(userID, id))
	}
	if len(keys) == 0 {
		return nil
	}
	return e.client.Del(call, keys...).Err()
}

// CountUser reports how many derived keys one user has.
func (e *EpisodeIndex) CountUser(ctx context.Context, userID string) (int, error) {
	count, _, err := e.scanUserKeys(ctx, userID)
	return count, err
}

// ResetUser removes the complete derived view for one user, including keys
// whose authoritative row disappeared while Redis was unavailable.
func (e *EpisodeIndex) ResetUser(ctx context.Context, userID string) error {
	_, keys, err := e.scanUserKeys(ctx, userID)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	if err := e.client.Del(call, keys...).Err(); err != nil {
		return describe(call, err)
	}
	return nil
}

func (e *EpisodeIndex) scanUserKeys(ctx context.Context, userID string) (int, []string, error) {
	if err := checkScopeValue("episode owner", userID); err != nil {
		return 0, nil, err
	}
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	pattern := e.keyPrefix + userID + ":*"
	var (
		cursor uint64
		total  int
		keys   []string
	)
	for {
		batch, next, err := e.client.Scan(call, cursor, pattern, episodeScanChunk).Result()
		if err != nil {
			return 0, nil, describe(call, err)
		}
		total += len(batch)
		keys = append(keys, batch...)
		if next == 0 {
			return total, keys, nil
		}
		cursor = next
	}
}

// SearchByVector runs the KNN channel inside one owner's scope, excluding the
// session the reader is in and any row whose vector came from a different
// embedder identity.
func (e *EpisodeIndex) SearchByVector(ctx context.Context, userID, currentSession, identity string, vector []float32, topK int) ([]EpisodeSearchResult, error) {
	if len(vector) != e.dimensions || topK <= 0 {
		return nil, fmt.Errorf("%w: invalid episode vector query", ErrConfig)
	}
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	filter, err := episodeScopeFilter(userID, currentSession, identity)
	if err != nil {
		return nil, err
	}
	statement := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS %s]", filter, topK, EpisodeFieldVector, episodeFieldDistance)
	result, err := e.client.FTSearchWithArgs(call, e.indexName, statement, &redis.FTSearchOptions{
		Return: []redis.FTSearchReturn{
			// The distance alias must be in the RETURN list: a KNN alias that is
			// computed but not returned leaves a hit with no score, and this
			// package refuses a hit with no score rather than defaulting it.
			{FieldName: EpisodeFieldEpisodeID}, {FieldName: EpisodeFieldLastMessageAt},
			{FieldName: episodeFieldDistance},
		},
		SortBy: []redis.FTSearchSortBy{{FieldName: episodeFieldDistance, Asc: true}},
		Limit:  topK, DialectVersion: 2,
		Params: map[string]interface{}{"query_vector": vectorBytes(vector)},
	}).Result()
	if err != nil {
		return nil, describe(call, err)
	}
	return parseEpisodeResults(result.Docs, true, false)
}

// SearchByText runs the lexical channel: the tokenized text fields plus the
// untokenized bigram tag. Both halves are one query, so a document hitting
// several bigrams still outranks one hitting a single bigram.
func (e *EpisodeIndex) SearchByText(ctx context.Context, userID, currentSession, identity string, terms []string, topK int) ([]EpisodeSearchResult, error) {
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	fields := EpisodeFieldTitle + "|" + EpisodeFieldSearchText
	clauses := make([]string, 0, len(terms)+1)
	for _, term := range terms {
		if strings.TrimSpace(term) != "" {
			clauses = append(clauses, fmt.Sprintf("@%s:(%s)", fields, escapeTagValue(term)))
		}
	}
	if bigrams := episodeBigramQueryTerms(terms); len(bigrams) > 0 {
		values := make([]string, 0, len(bigrams))
		for _, bigram := range bigrams {
			values = append(values, escapeTagValue(bigram))
		}
		clauses = append(clauses, fmt.Sprintf("@%s:{%s}", EpisodeFieldSearchBigrams, strings.Join(values, "|")))
	}
	if len(clauses) == 0 {
		return []EpisodeSearchResult{}, nil
	}
	filter, err := episodeScopeFilter(userID, currentSession, identity)
	if err != nil {
		return nil, err
	}
	statement := fmt.Sprintf("(%s) (%s)", filter, strings.Join(clauses, "|"))
	result, err := e.client.FTSearchWithArgs(call, e.indexName, statement, &redis.FTSearchOptions{
		Return: []redis.FTSearchReturn{
			{FieldName: EpisodeFieldEpisodeID}, {FieldName: EpisodeFieldLastMessageAt},
		},
		WithScores: true, Limit: topK, DialectVersion: 2, Language: "chinese", Scorer: "BM25STD",
	}).Result()
	if err != nil {
		return nil, describe(call, err)
	}
	return parseEpisodeResults(result.Docs, false, false)
}

// SearchExactTopics runs the exact channel: a TAG match over normalized topic
// keys, newest first.
func (e *EpisodeIndex) SearchExactTopics(ctx context.Context, userID, currentSession, identity string, topics []string, topK int) ([]EpisodeSearchResult, error) {
	values := make([]string, 0, len(topics))
	for _, topic := range topics {
		if strings.TrimSpace(topic) != "" {
			values = append(values, escapeTagValue(topic))
		}
	}
	if len(values) == 0 {
		return []EpisodeSearchResult{}, nil
	}
	call, cancel := e.contextWithTTL(ctx)
	defer cancel()
	filter, err := episodeScopeFilter(userID, currentSession, identity)
	if err != nil {
		return nil, err
	}
	statement := fmt.Sprintf("%s @%s:{%s}", filter, EpisodeFieldTopicKeys, strings.Join(values, "|"))
	result, err := e.client.FTSearchWithArgs(call, e.indexName, statement, &redis.FTSearchOptions{
		Return: []redis.FTSearchReturn{
			{FieldName: EpisodeFieldEpisodeID}, {FieldName: EpisodeFieldLastMessageAt},
		},
		SortBy: []redis.FTSearchSortBy{{FieldName: EpisodeFieldLastMessageAt, Asc: false}},
		Limit:  topK, DialectVersion: 2,
	}).Result()
	if err != nil {
		return nil, describe(call, err)
	}
	return parseEpisodeResults(result.Docs, false, true)
}

func episodeScopeFilter(userID, currentSession, identity string) (string, error) {
	if err := checkScopeValue("episode owner", strings.TrimSpace(userID)); err != nil {
		return "", err
	}
	if identity == "" {
		identity = unknownIdentity
	}
	if err := checkScopeValue("episode vector identity", identity); err != nil {
		return "", err
	}
	filter := fmt.Sprintf("@%s:{%s} @%s:{%s} @%s:{%s}",
		EpisodeFieldUserID, escapeTagValue(strings.TrimSpace(userID)),
		EpisodeFieldStatus, "active",
		EpisodeFieldIdentity, escapeTagValue(identity))
	if currentSession != "" {
		filter += fmt.Sprintf(" -@%s:{%s}", EpisodeFieldSessionID, escapeTagValue(currentSession))
	}
	return filter, nil
}

func parseEpisodeResults(docs []redis.Document, distance, preserveOrder bool) ([]EpisodeSearchResult, error) {
	results := make([]EpisodeSearchResult, 0, len(docs))
	for _, doc := range docs {
		id := doc.Fields[EpisodeFieldEpisodeID]
		if id == "" {
			return nil, fmt.Errorf("%w: an episode hit carried no id", ErrSchema)
		}
		item := EpisodeSearchResult{EpisodeID: id}
		if unix, err := strconv.ParseInt(doc.Fields[EpisodeFieldLastMessageAt], 10, 64); err == nil {
			item.LastMessageAt = unix
		}
		if doc.Score != nil {
			item.Score = *doc.Score
		}
		if distance {
			raw, present := doc.Fields[episodeFieldDistance]
			if !present {
				return nil, fmt.Errorf("%w: an episode hit carried no distance", ErrSchema)
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				return nil, fmt.Errorf("%w: episode distance %q is not a number", ErrSchema, raw)
			}
			item.Distance = parsed
		}
		results = append(results, item)
	}
	if !preserveOrder {
		sort.SliceStable(results, func(i, j int) bool {
			if distance && results[i].Distance != results[j].Distance {
				return results[i].Distance < results[j].Distance
			}
			if !distance && results[i].Score != results[j].Score {
				return results[i].Score > results[j].Score
			}
			if results[i].LastMessageAt != results[j].LastMessageAt {
				return results[i].LastMessageAt > results[j].LastMessageAt
			}
			return results[i].EpisodeID < results[j].EpisodeID
		})
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results, nil
}

// Close releases the pooled connection.
func (e *EpisodeIndex) Close() error {
	if e == nil || e.client == nil {
		return nil
	}
	return e.client.Close()
}

// episodeBigrams returns the distinct adjacent Han character pairs in a text.
//
// RediSearch tokenizes Chinese with a dictionary that does not contain this
// project's vocabulary, so a search for a technical term matches nothing while
// its characters match everything. Bigrams sidestep the dictionary: any term of
// two or more Han characters contains at least one indexed pair. They live in a
// TAG field because a TEXT field would re-tokenize them on the way in.
func episodeBigrams(value string) []string {
	runes := []rune(value)
	seen := make(map[string]struct{}, len(runes))
	bigrams := make([]string, 0, len(runes))
	for index := 0; index+1 < len(runes) && len(bigrams) < episodeMaxIndexedBigrams; index++ {
		if !unicode.Is(unicode.Han, runes[index]) || !unicode.Is(unicode.Han, runes[index+1]) {
			continue
		}
		pair := string(runes[index : index+2])
		if _, exists := seen[pair]; exists {
			continue
		}
		seen[pair] = struct{}{}
		bigrams = append(bigrams, pair)
	}
	return bigrams
}

func episodeBigramTagValue(value string) string {
	return strings.Join(episodeBigrams(value), ",")
}

// episodeBigramQueryTerms keeps only the Han bigrams of an already-extracted
// term list, which is what the TAG field can match. Longer Han terms and ASCII
// identifiers are for the tokenized text field and would never match a tag.
func episodeBigramQueryTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		runes := []rune(term)
		if len(runes) != 2 || !unicode.Is(unicode.Han, runes[0]) || !unicode.Is(unicode.Han, runes[1]) {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	return out
}

// --- FT.INFO schema verification -------------------------------------------
//
// The structured client result does not carry TAG separators, VECTOR arguments
// or the default language, so the verification reads the raw FT.INFO map and
// compares every attribute this schema would create.

func verifyEpisodeSchema(indexName, keyPrefix string, dimensions int, info map[string]any) error {
	attributes, _ := info["attributes"].([]any)
	expected := expectedEpisodeAttributes(dimensions)
	byName := map[string]map[string]any{}
	for _, want := range expected {
		byName[want["attribute"].(string)] = want
	}
	seen := map[string]bool{}
	for _, raw := range attributes {
		entries, isSlice := raw.([]any)
		var attribute map[string]any
		if isSlice {
			attribute = episodeInfoMap(entries)
		} else {
			attribute, _ = raw.(map[string]any)
		}
		name, _ := attribute["attribute"].(string)
		if name == "" {
			name, _ = attribute["identifier"].(string)
		}
		want, known := byName[name]
		if !known {
			return fmt.Errorf("%w: episode index %s carries unexpected field %s", ErrSchema, indexName, name)
		}
		seen[name] = true
		if got, _ := attribute["type"].(string); got != want["type"] {
			return fmt.Errorf("%w: episode index %s field %s is type %q, want %q", ErrSchema, indexName, name, got, want["type"])
		}
		if wantSortable, _ := want["sortable"].(bool); wantSortable {
			if _, present := attribute["SORTABLE"]; !present {
				return fmt.Errorf("%w: episode index %s field %s is not sortable", ErrSchema, indexName, name)
			}
		}
		if wantNoStem, _ := want["nostem"].(bool); wantNoStem {
			if _, present := attribute["NOSTEM"]; !present {
				return fmt.Errorf("%w: episode index %s field %s is not NOSTEM", ErrSchema, indexName, name)
			}
		}
		if wantWeight, ok := want["weight"].(float64); ok && wantWeight > 1 {
			gotWeight, _ := strconv.ParseFloat(fmt.Sprintf("%v", attribute["WEIGHT"]), 64)
			if attribute["WEIGHT"] != nil && gotWeight != wantWeight {
				return fmt.Errorf("%w: episode index %s field %s weight = %v, want %v", ErrSchema, indexName, name, attribute["WEIGHT"], wantWeight)
			}
		}
		if wantSeparator, _ := want["separator"].(string); wantSeparator != "" {
			got, _ := attribute["SEPARATOR"].(string)
			if got != wantSeparator {
				return fmt.Errorf("%w: episode index %s field %s separator = %q, want %q", ErrSchema, indexName, name, got, wantSeparator)
			}
		}
		if vectorArgs, ok := want["vector"].([]string); ok {
			if got, _ := attribute["algorithm"].(string); got != vectorArgs[0] {
				return fmt.Errorf("%w: episode index %s vector algorithm = %q, want %q", ErrSchema, indexName, got, vectorArgs[0])
			}
			if got, _ := attribute["data_type"].(string); got != vectorArgs[1] {
				return fmt.Errorf("%w: episode index %s vector data_type = %q, want %q", ErrSchema, indexName, got, vectorArgs[1])
			}
			gotDim, _ := attribute["dim"].(int64)
			if strconv.FormatInt(gotDim, 10) != vectorArgs[2] {
				return fmt.Errorf("%w: episode index %s vector dim = %d, want %s", ErrSchema, indexName, gotDim, vectorArgs[2])
			}
			if got, _ := attribute["distance_metric"].(string); got != vectorArgs[3] {
				return fmt.Errorf("%w: episode index %s vector distance = %q, want %q", ErrSchema, indexName, got, vectorArgs[3])
			}
		}
	}
	for _, want := range expected {
		name := want["attribute"].(string)
		if !seen[name] {
			return fmt.Errorf("%w: episode index %s is missing field %s", ErrSchema, indexName, name)
		}
	}
	rawPrefixes, _ := info["index_definition"].(map[string]any)
	prefixes, _ := rawPrefixes["prefixes"].([]any)
	if len(prefixes) != 1 {
		return fmt.Errorf("%w: episode index %s covers %d prefixes, want exactly 1", ErrSchema, indexName, len(prefixes))
	}
	prefix, _ := prefixes[0].(string)
	if prefix != keyPrefix {
		return fmt.Errorf("%w: episode index %s covers prefix %q, want %q", ErrSchema, indexName, prefix, keyPrefix)
	}
	if lang, _ := rawPrefixes["default_language"].(string); lang != "chinese" {
		return fmt.Errorf("%w: episode index %s default language = %q, want chinese", ErrSchema, indexName, lang)
	}
	return nil
}

func expectedEpisodeAttributes(dimensions int) []map[string]any {
	return []map[string]any{
		{"attribute": EpisodeFieldEpisodeID, "type": "TAG"},
		{"attribute": EpisodeFieldUserID, "type": "TAG"},
		{"attribute": EpisodeFieldSessionID, "type": "TAG"},
		{"attribute": EpisodeFieldStatus, "type": "TAG"},
		{"attribute": EpisodeFieldLastMessageAt, "type": "NUMERIC", "sortable": true},
		{"attribute": EpisodeFieldTitle, "type": "TEXT", "weight": 3.0, "nostem": true},
		{"attribute": EpisodeFieldTopicKeys, "type": "TAG"},
		{"attribute": EpisodeFieldSearchText, "type": "TEXT", "nostem": true},
		{"attribute": EpisodeFieldSearchBigrams, "type": "TAG", "separator": ","},
		{"attribute": EpisodeFieldIdentity, "type": "TAG"},
		{"attribute": EpisodeFieldVector, "type": "VECTOR",
			"vector": []string{"FLAT", "FLOAT32", strconv.Itoa(dimensions), "COSINE"}},
	}
}

// episodeInfoMap folds the flat key/value slice a generic FT.INFO returns into
// the nested map shape the verifier walks. A trailing unpaired key is a flag
// Redis emits with no value (NOSTEM, SORTABLE); it is stored as a
// present-with-nil entry so presence checks still work.
func episodeInfoMap(flat []any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(flat); i += 2 {
		key, _ := flat[i].(string)
		switch value := flat[i+1].(type) {
		case []any:
			out[key] = episodeInfoMapSlice(value)
		default:
			out[key] = value
		}
	}
	if len(flat)%2 == 1 {
		if key, ok := flat[len(flat)-1].(string); ok {
			out[key] = nil
		}
	}
	return out
}

func episodeInfoMapSlice(entries []any) any {
	if len(entries)%2 != 0 {
		return entries
	}
	keysOnly := true
	for i := 0; i < len(entries); i += 2 {
		if _, ok := entries[i].(string); !ok {
			keysOnly = false
			break
		}
	}
	if !keysOnly {
		return entries
	}
	guess := map[string]any{}
	for i := 0; i+1 < len(entries); i += 2 {
		key, _ := entries[i].(string)
		switch value := entries[i+1].(type) {
		case []any:
			guess[key] = episodeInfoMapSlice(value)
		default:
			guess[key] = value
		}
	}
	return guess
}
