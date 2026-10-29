// Package redisindex keeps chunk vectors in Redis Stack / RediSearch so a semantic
// query costs one embedding and one KNN search instead of one embedding per stored
// chunk.
//
// The schema and the query string follow the shape the original project ran, because
// that combination is what its corpus was measured against: one shared index per
// deployment, owner isolation enforced by TAG filters at query time, FLAT rather than
// HNSW so a recall is exact at the sizes this project stores, FLOAT32 COSINE, the
// Chinese analyzer, and a distance returned as an explicit named field.
//
// One deliberate difference: the original defaulted a missing distance to 1.0, which
// reads as "very far" and silently drops the hit. Here a missing distance is an error,
// because a hit with no score means the RETURN list and the index disagree, and every
// later answer built on that pairing would quietly lose a chunk.
package redisindex

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	redis "github.com/redis/go-redis/v9"

	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

const (
	// DefaultIndexName is one versioned index per schema, so a schema change creates a
	// new name instead of leaving a half-populated old one behind.
	DefaultIndexName = "gopherlearn_knowledge_chunks_v1_idx"
	// DefaultKeyPrefix scopes every hashed chunk under one prefix the index walks.
	DefaultKeyPrefix = "gopherlearn:knowledge:chunk:"
	// scanChunk bounds each SCAN/DEL batch: a full sweep of a large document must not
	// hold a single Redis command open for thousands of keys.
	scanChunk = 200
	// DefaultOperationTimeout bounds one index command when the caller supplied no
	// deadline. It is generous enough for a batched write and short enough that a dead
	// Redis is reported rather than hung.
	DefaultOperationTimeout = 10 * time.Second
)

// Field names are exported so a test and an operator can name the exact schema.
const (
	FieldUserID     = "user_id"
	FieldCollection = "collection_id"
	FieldDocument   = "doc_id"
	FieldVersion    = "doc_version"
	FieldChunkID    = "chunk_id"
	FieldChunkOrder = "chunk_order"
	FieldIdentity   = "identity"
	FieldVector     = "vector"
	fieldDistance   = "distance"
	unknownIdentity = "-"
)

var (
	// ErrConfig reports an index that cannot be built. Callers must fail at startup: a
	// semantic mode that silently stopped writing vectors would keep answering from an
	// empty index and look like a retrieval-quality problem.
	ErrConfig = errors.New("redisindex: configuration is invalid")
	// ErrUnavailable reports that Redis could not be reached or answered with a
	// server-side failure.
	ErrUnavailable = errors.New("redisindex: redis unavailable")
	// ErrSchema reports an index whose answers cannot be interpreted: a missing
	// distance, a score that is not a number.
	ErrSchema = errors.New("redisindex: search result does not match the schema")
)

// Options configures an Index.
type Options struct {
	// Address is host:port. A url form (redis://host:port) is accepted so an operator
	// can paste the same value the compose stack publishes.
	Address      string
	Password     string
	Database     int
	IndexName    string
	KeyPrefix    string
	Dimensions   int
	OperationTTL time.Duration
}

// Index is the RediSearch-backed implementation of retrieval.VectorIndex.
type Index struct {
	client       *redis.Client
	indexName    string
	keyPrefix    string
	dimensions   int
	operationTTL time.Duration
}

var _ retrieval.VectorIndex = (*Index)(nil)

// New builds the client. It performs no network I/O: connection failures surface from
// the first command, which is what lets a deployment start while Redis is still
// warming up and fail on the operation that actually needed it.
func New(options Options) (*Index, error) {
	host, password, database, err := resolveConnection(options.Address, options.Password, options.Database)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if options.Dimensions <= 0 {
		return nil, fmt.Errorf("%w: dimensions must be positive", ErrConfig)
	}
	name := strings.TrimSpace(options.IndexName)
	if name == "" {
		name = DefaultIndexName
	}
	prefix := strings.TrimSpace(options.KeyPrefix)
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	ttl := options.OperationTTL
	if ttl <= 0 {
		ttl = DefaultOperationTimeout
	}
	return &Index{
		client: redis.NewClient(&redis.Options{
			Addr: host, Password: password, DB: database, Protocol: 2,
		}),
		indexName: name, keyPrefix: prefix, dimensions: options.Dimensions, operationTTL: ttl,
	}, nil
}

// resolveConnection accepts "host:port", ":port" or a redis:// URL and fills the port
// Redis actually listens on, so a pasted URL and a pasted host:port behave the same.
func resolveConnection(address, password string, database int) (string, string, int, error) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "", "", 0, errors.New("address is required")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", 0, errors.New("address is not parseable")
		}
		if parsed.Scheme != "redis" && parsed.Scheme != "rediss" {
			return "", "", 0, errors.New("address scheme must be redis or rediss")
		}
		if parsed.User != nil {
			return "", "", 0, errors.New("credentials must be supplied separately, not in the address")
		}
		if parsed.Path != "" && parsed.Path != "/" {
			// Measured, not defensive: RediSearch refuses FT.CREATE on any database
			// other than 0, so a URL that selects one cannot describe a usable index.
			return "", "", 0, errors.New("a database index in the path is not supported: the search module only indexes database 0")
		}
		if port := parsed.Port(); port == "" {
			trimmed = parsed.Hostname() + ":6379"
		} else {
			trimmed = parsed.Host
		}
	}
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", "", 0, errors.New("address must be host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "", 0, errors.New("port must be a number between 1 and 65535")
	}
	if host == "" {
		// ":6380" is how an operator writes "this machine, that port" in every other
		// Redis connection string, so it means localhost rather than an empty host.
		host = "127.0.0.1"
	}
	// Found by running against a real server, not by reading the docs: FT.CREATE answers
	// "Cannot create index on db != 0" for any other database, so a non-zero selection is
	// not a preference that works less well - it is an index that can never exist. It has
	// to be refused here, because otherwise the failure surfaces on a learner's first
	// document upload, several steps away from the setting that caused it.
	if database != 0 {
		return "", "", 0, errors.New("database must be 0: the search module only indexes database 0, so isolation comes from the key prefix and the index name")
	}
	return net.JoinHostPort(host, port), password, database, nil
}

// Dimensions reports the width this index was built for.
func (i *Index) Dimensions() int { return i.dimensions }

// Name reports the index this client queries, after the default has been applied. The
// startup line prints it rather than re-deriving it, so a deployment that overrode
// GOPHERLEARN_REDIS_INDEX_NAME cannot end up logging a different index than the one it is
// reading.
func (i *Index) Name() string { return i.indexName }

// contextWithTTL gives every command a deadline even when the caller passed a bare
// context, so a hung Redis cannot hold a learner's request open indefinitely.
func (i *Index) contextWithTTL(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, set := ctx.Deadline(); set {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, i.operationTTL)
}

// EnsureIndex creates the index when it is absent and accepts an existing one, which
// is the normal state after the first boot and when two instances start at once.
//
// Accepting an existing index is only safe if it is the same index, so every call ends with
// a probe query: a schema left behind by a different vector width, or a module version that
// parses the query this build sends differently, becomes a startup failure instead of a
// learner's upload that indexes fine and a search that answers nothing.
func (i *Index) EnsureIndex(ctx context.Context) error {
	call, cancel := i.contextWithTTL(ctx)
	defer cancel()
	if _, err := i.client.FTInfo(call, i.indexName).Result(); err != nil {
		if !isUnknownIndexError(err) {
			return describe(call, err)
		}
		if err := i.createIndex(call); err != nil {
			return err
		}
	}
	return i.probeQuery(call)
}

// createIndex issues FT.CREATE and treats "it already exists" as success, because two
// instances starting together is a normal sequence and only one of them can win.
func (i *Index) createIndex(call context.Context) error {
	schema := []*redis.FieldSchema{
		{FieldName: FieldUserID, FieldType: redis.SearchFieldTypeTag},
		{FieldName: FieldCollection, FieldType: redis.SearchFieldTypeTag},
		{FieldName: FieldDocument, FieldType: redis.SearchFieldTypeTag},
		{FieldName: FieldVersion, FieldType: redis.SearchFieldTypeNumeric},
		{FieldName: FieldChunkID, FieldType: redis.SearchFieldTypeTag},
		{FieldName: FieldChunkOrder, FieldType: redis.SearchFieldTypeNumeric, Sortable: true},
		// The embedder fingerprint is a TAG so a query can filter on it. Refusing to
		// answer across identities is what turns "the model changed" from a silent
		// accuracy drop into an explicit re-index.
		{FieldName: FieldIdentity, FieldType: redis.SearchFieldTypeTag},
		{
			FieldName: FieldVector, FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{FlatOptions: &redis.FTFlatOptions{
				Type: "FLOAT32", Dim: i.dimensions, DistanceMetric: "COSINE",
			}},
		},
	}
	options := &redis.FTCreateOptions{
		OnHash:          true,
		Prefix:          []interface{}{i.keyPrefix},
		DefaultLanguage: "chinese",
	}
	if err := i.client.FTCreate(call, i.indexName, options, schema...).Err(); err != nil {
		if isIndexAlreadyExistsError(err) {
			return nil
		}
		return describe(call, err)
	}
	return nil
}

// probeQuery asks the index the one question the whole mode depends on, in the shape the
// retrieval layer will use it: a KNN clause at the configured width, over every document.
//
// The width is the load-bearing part. An index created by a build with a different
// DIMENSION value accepts writes of its own width and rejects this process's queries with
// "query vector blob size (16) does not match index's expected size (32)" - which is loud,
// but only on a learner's first question, several steps from the configuration that caused
// it. Running the probe here turns that into a startup failure, and also proves the module
// answering is one that parses the query text this build sends.
func (i *Index) probeQuery(call context.Context) error {
	probe := make([]float32, i.dimensions)
	probe[0] = 1
	statement := fmt.Sprintf("*=>[KNN 1 @%s $probe_vector AS %s]", FieldVector, fieldDistance)
	if err := i.client.FTSearchWithArgs(call, i.indexName, statement, &redis.FTSearchOptions{
		Limit: 1, DialectVersion: 2,
		Params: map[string]interface{}{"probe_vector": vectorBytes(probe)},
	}).Err(); err != nil {
		return fmt.Errorf("%w: index %q does not answer a %d-dimensional query: %v",
			ErrConfig, i.indexName, i.dimensions, describe(call, err))
	}
	return nil
}

// Key builds the hash key for one chunk version. It is exported for tests and for the
// audit that checks the index and the store agree.
//
// The identifiers go in as they are: a key is an opaque string to Redis, and the escape
// the query language needs would just be bytes nobody can search for later. The values are
// refused upstream instead (checkKeyValue), which is what keeps the SCAN pattern in
// deleteMatching and the keys it matches built the same way.
func (i *Index) Key(userID, documentID string, version int, chunkID string) string {
	return i.keyPrefix + userID + ":" + documentID + ":" + strconv.Itoa(version) + ":" + chunkID
}

// Upsert writes vectors for the given chunks. The full field set is rewritten every
// time rather than merged, so a chunk whose section or order changed cannot keep a
// stale attribute beside a fresh vector.
func (i *Index) Upsert(ctx context.Context, userID string, entries []retrieval.VectorEntry) error {
	if len(entries) == 0 {
		return nil
	}
	call, cancel := i.contextWithTTL(ctx)
	defer cancel()
	pipe := i.client.TxPipeline()
	if err := checkKeyValue("owner", userID); err != nil {
		return err
	}
	for _, entry := range entries {
		if len(entry.Vector) != i.dimensions {
			return fmt.Errorf("%w: chunk %s carries %d dimensions, index holds %d",
				ErrConfig, entry.Chunk.ID, len(entry.Vector), i.dimensions)
		}
		// Every one of these ends up in either a key segment or a TAG clause, so a value
		// that would need quoting is refused at the door instead of being re-interpreted
		// by the next reader's filter or the next sweep's pattern.
		for _, scope := range []struct {
			kind  string
			value string
			inKey bool
		}{
			{"collection id", entry.Chunk.CollectionID, false},
			{"document id", entry.Chunk.DocumentID, true},
			{"chunk id", entry.Chunk.ID, true},
			{"vector identity", orUnknown(entry.Identity), false},
		} {
			check := checkScopeValue
			if scope.inKey {
				check = checkKeyValue
			}
			if err := check(scope.kind, scope.value); err != nil {
				return err
			}
		}
		pipe.HSet(call, i.Key(userID, entry.Chunk.DocumentID, entry.Chunk.Version, entry.Chunk.ID), map[string]any{
			FieldUserID: userID, FieldCollection: entry.Chunk.CollectionID,
			FieldDocument: entry.Chunk.DocumentID, FieldVersion: entry.Chunk.Version,
			FieldChunkID: entry.Chunk.ID, FieldChunkOrder: entry.Chunk.Order,
			FieldIdentity: orUnknown(entry.Identity), FieldVector: vectorBytes(entry.Vector),
		})
	}
	if _, err := pipe.Exec(call); err != nil {
		return describe(call, err)
	}
	return nil
}

// DeleteDocument removes every version of one document's vectors.
func (i *Index) DeleteDocument(ctx context.Context, userID string, documentID string) error {
	return i.deleteMatching(ctx, userID, documentID, 0)
}

// DeleteVersions removes every version of one document except the one that is now
// current, which is how a replaced document stops being retrievable at its old text
// while the new rows stay live.
func (i *Index) DeleteVersions(ctx context.Context, userID string, documentID string, keepVersion int) error {
	return i.deleteMatching(ctx, userID, documentID, keepVersion)
}

func (i *Index) deleteMatching(ctx context.Context, userID, documentID string, keepVersion int) error {
	call, cancel := i.contextWithTTL(ctx)
	defer cancel()
	// The raw values go into the pattern because a backslash means something to SCAN's own
	// glob, and checkKeyValue has already refused the characters that would need one. An
	// identifier carrying '*' would otherwise turn a delete of one document into a delete of
	// everybody's.
	if err := checkKeyValue("owner", userID); err != nil {
		return err
	}
	if err := checkKeyValue("document id", documentID); err != nil {
		return err
	}
	pattern := i.keyPrefix + userID + ":" + documentID + ":*"
	var (
		cursor  uint64
		removed int
		batch   []string
	)
	for {
		keys, next, err := i.client.Scan(call, cursor, pattern, scanChunk).Result()
		if err != nil {
			return describe(call, err)
		}
		for _, key := range keys {
			if keepVersion > 0 && versionOfKey(key) == keepVersion {
				continue
			}
			batch = append(batch, key)
			if len(batch) >= scanChunk {
				if err := i.client.Del(call, batch...).Err(); err != nil {
					return describe(call, err)
				}
				removed += len(batch)
				batch = batch[:0]
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if len(batch) > 0 {
		if err := i.client.Del(call, batch...).Err(); err != nil {
			return describe(call, err)
		}
		removed += len(batch)
	}
	_ = removed
	return nil
}

// Search asks for the nearest neighbours of one query vector inside the caller's scope.
func (i *Index) Search(ctx context.Context, query retrieval.VectorSearch) ([]retrieval.VectorHit, error) {
	if len(query.Vector) != i.dimensions {
		return nil, fmt.Errorf("%w: query carries %d dimensions, index holds %d",
			ErrConfig, len(query.Vector), i.dimensions)
	}
	call, cancel := i.contextWithTTL(ctx)
	defer cancel()
	limit := query.Limit
	if limit <= 0 {
		limit = retrieval.DefaultRecallLimit
	}
	filter, err := scopeFilter(query)
	if err != nil {
		return nil, err
	}
	statement := fmt.Sprintf("(%s)=>[KNN %d @%s $query_vector AS %s]",
		filter, limit, FieldVector, fieldDistance)
	options := &redis.FTSearchOptions{
		Return: []redis.FTSearchReturn{{FieldName: FieldChunkID}, {FieldName: fieldDistance}},
		SortBy: []redis.FTSearchSortBy{{FieldName: fieldDistance, Asc: true}},
		Limit:  limit, DialectVersion: 2,
		Params: map[string]interface{}{"query_vector": vectorBytes(query.Vector)},
	}
	result, err := i.client.FTSearchWithArgs(call, i.indexName, statement, options).Result()
	if err != nil {
		return nil, describe(call, err)
	}
	return parseHits(result.Docs)
}

// Close releases the pooled connection.
func (i *Index) Close() error {
	if i == nil || i.client == nil {
		return nil
	}
	return i.client.Close()
}

// scopeFilter builds the TAG filter that keeps a search inside one owner, the
// collections named by the turn, and the embedder identity that produced the vectors.
func scopeFilter(query retrieval.VectorSearch) (string, error) {
	if err := checkScopeValue("owner", strings.TrimSpace(query.UserID)); err != nil {
		return "", err
	}
	if len(query.CollectionIDs) == 0 {
		return "", fmt.Errorf("%w: a search needs a collection scope", ErrConfig)
	}
	collections := make([]string, 0, len(query.CollectionIDs))
	seen := make(map[string]struct{}, len(query.CollectionIDs))
	for _, collection := range query.CollectionIDs {
		trimmed := strings.TrimSpace(collection)
		if trimmed == "" {
			continue
		}
		if err := checkScopeValue("collection id", trimmed); err != nil {
			return "", err
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		collections = append(collections, escapeTagValue(trimmed))
	}
	if len(collections) == 0 {
		return "", fmt.Errorf("%w: a search needs a collection scope", ErrConfig)
	}
	sort.Strings(collections)
	identity := query.Identity
	if identity == "" {
		identity = unknownIdentity
	}
	if err := checkScopeValue("vector identity", identity); err != nil {
		return "", err
	}
	return fmt.Sprintf("@%s:{%s} @%s:{%s} @%s:{%s}",
		FieldUserID, escapeTagValue(strings.TrimSpace(query.UserID)),
		FieldCollection, strings.Join(collections, "|"),
		FieldIdentity, escapeTagValue(identity)), nil
}

func parseHits(documents []redis.Document) ([]retrieval.VectorHit, error) {
	hits := make([]retrieval.VectorHit, 0, len(documents))
	for _, document := range documents {
		fields := document.Fields
		chunkID := strings.TrimSpace(fields[FieldChunkID])
		if chunkID == "" {
			return nil, fmt.Errorf("%w: a hit carried no chunk id", ErrSchema)
		}
		raw, present := fields[fieldDistance]
		if !present {
			// The original defaulted this to 1.0; see the package comment for why that
			// is a silent data loss rather than a conservative choice.
			return nil, fmt.Errorf("%w: a hit carried no distance", ErrSchema)
		}
		distance, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: distance %q is not a number", ErrSchema, raw)
		}
		if math.IsNaN(distance) || math.IsInf(distance, 0) {
			return nil, fmt.Errorf("%w: distance is not finite", ErrSchema)
		}
		hits = append(hits, retrieval.VectorHit{ChunkID: chunkID, Similarity: retrieval.SimilarityFromCosineDistance(distance)})
	}
	return hits, nil
}

// vectorBytes packs a float32 vector little-endian, which is the byte layout
// RediSearch expects for FLOAT32.
func vectorBytes(vector []float32) []byte {
	buf := make([]byte, 4*len(vector))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(buf[4*index:], math.Float32bits(value))
	}
	return buf
}

// escapeTagValue protects a TAG clause from a value that would be read as syntax.
//
// The set is measured against a real server, not copied from a list of plausible
// characters: an unescaped '-' inside a tag literal is not treated as ordinary text, it
// fails the query with "Syntax error at offset 14 near user", and escaping it as `\-`
// both parses and matches the stored value. Since every identifier this project generates
// contains a dash, the unescaped version made the first real search fail outright. The
// other characters here either end the clause early ('}', '|'), act as a wildcard or a
// negation ('*', '?'), or are the TAG field's own value separator (',').
//
// Escaping a character that carries no meaning is not a risk: the round-trip cases in the
// gated suite assert that a value which really does contain each of them is still found,
// which is the property that would break if this list were wrong in the other direction.
func escapeTagValue(value string) string {
	const specials = `,.;:!?|/\{}~*+()[]<>=&%#@$^-`
	var builder strings.Builder
	for _, character := range value {
		if strings.ContainsRune(specials, character) {
			builder.WriteRune('\\')
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

// tagSeparator is the character a TAG field itself splits values on. It is not in the
// escape list's safe territory: a stored value of "a,b" becomes the two tags a and b, so a
// search for either one matches it. Nothing about this project's identifiers produces a
// comma, so the value that would exploit that is refused at the door rather than indexed
// and then quietly matched by somebody else's query.
const tagSeparator = ','

// keySeparator splits the segments of an index key.
const keySeparator = ':'

// keyGlobMetacharacters are the characters SCAN interprets in a match pattern. Deletion
// finds the keys of one document with a pattern built from an identifier, so an identifier
// carrying '*' or '?' would widen a delete into another owner's vectors - a data-losing
// version of the same mistake the tag separator guard above prevents.
const keyGlobMetacharacters = `*?[]\`

// checkScopeValue rejects an identifier that cannot be stored and searched without
// changing what some other query matches. It runs on the write path so the failure lands
// on the upload that caused it, not on a later reader's answer.
func checkScopeValue(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is empty", ErrConfig, kind)
	}
	if strings.ContainsRune(value, tagSeparator) {
		return fmt.Errorf("%w: %s contains %q, which the search index would store as two separate tags",
			ErrConfig, kind, string(tagSeparator))
	}
	if strings.ContainsAny(value, keyGlobMetacharacters) {
		return fmt.Errorf("%w: %s contains a pattern character from %q, which the index would have to match literally",
			ErrConfig, kind, keyGlobMetacharacters)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s has surrounding whitespace, which the index would trim", ErrConfig, kind)
	}
	return nil
}

// checkKeyValue adds the constraint the key layout imposes on top of checkScopeValue:
// segments are separated by ':', and the version segment is found by position when a sweep
// decides what to keep. An identifier containing a separator would be read as two segments
// and could make a current version look like an old one.
func checkKeyValue(kind, value string) error {
	if err := checkScopeValue(kind, value); err != nil {
		return err
	}
	if strings.ContainsRune(value, keySeparator) {
		return fmt.Errorf("%w: %s contains %q, which separates the segments of an index key",
			ErrConfig, kind, string(keySeparator))
	}
	return nil
}

// versionOfKey reads the version segment out of a chunk key. A malformed key is
// reported as version 0, which means "delete it": nothing legitimate lives there.
func versionOfKey(key string) int {
	segments := strings.Split(key, ":")
	if len(segments) == 0 {
		return 0
	}
	candidate := segments[len(segments)-2]
	version, err := strconv.Atoi(candidate)
	if err != nil {
		return 0
	}
	return version
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return unknownIdentity
	}
	return value
}

func isUnknownIndexError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unknown index") || errors.Is(err, redis.Nil)
}

func isIndexAlreadyExistsError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "index already exists")
}

// describe classifies a Redis error into the two kinds a caller can act on: the server
// is unreachable, or the request was refused. The upstream text is kept because it
// names the index and the field, and carries no user content.
func describe(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr net.Error
	if errors.As(err, &netErr) || strings.Contains(strings.ToLower(err.Error()), "connection refused") ||
		strings.Contains(strings.ToLower(err.Error()), "i/o timeout") {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return err
}
