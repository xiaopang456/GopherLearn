package redis

// The thin card's index.
//
// It is a text index and NOT a vector index, and that is the whole point of
// it.
//
// A vector index needs an embedding, and an embedding is a model call. The
// thin card's whole claim is that keeping a card current costs no model call
// at all - so the card is indexed by its words and by nothing else.
//
// The words are the learner's own: the session's title, the topics its
// summary segments recorded, the names they recorded. A question about an
// earlier session is phrased in those words ("上次那三道题", "上周聊的
// 缓存"), and a text index finds a card by exactly that.
//
// The field names are the episode index's names, because a thin card is a
// session card row and the two instruments read alike. The tag field that
// carries the Chinese bigrams is here for the same reason it is there: the
// Chinese tokenizer cannot match a term its dictionary does not hold.

// The thin cards' index namespace.
const (
	ThinCardIndexName = "gopherai_thin_card_v1_idx"
	ThinCardKeyPrefix = "gopherai:thin_card:vec:"
)

// ThinCardIndexSpec is the thin card instrument's schema.
//
// There is no vector field, and that is deliberate: see the file's comment.
func ThinCardIndexSpec(indexName, keyPrefix string) VectorIndexSpec {
	return VectorIndexSpec{
		Label:           "thin session card",
		Name:            indexName,
		KeyPrefix:       keyPrefix,
		DefaultLanguage: "chinese",
		Fields: []VectorIndexField{
			{Name: EpisodeFieldEpisodeID, Type: "TAG"},
			{Name: EpisodeFieldUserID, Type: "TAG"},
			{Name: EpisodeFieldSessionID, Type: "TAG"},
			{Name: EpisodeFieldStatus, Type: "TAG"},
			{Name: EpisodeFieldLastMessageAt, Type: "NUMERIC", Sortable: true},
			{Name: EpisodeFieldTitle, Type: "TEXT", Weight: 3, NoStem: true},
			{Name: EpisodeFieldTopicKeys, Type: "TAG"},
			{Name: EpisodeFieldSearchText, Type: "TEXT", NoStem: true},
			// The bigrams are a tag field for the reason the episode index's
			// are: the Chinese tokenizer would split them back into single
			// characters.
			{Name: EpisodeFieldSearchBigrams, Type: "TAG", Separator: ","},
		},
	}
}
