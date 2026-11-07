package conversation

import (
	"reflect"
	"testing"
)

// The arm's tool table follows the product's own registration rule: the
// browse tool exists only in the applied mode, because there are no segments
// to browse in the other two. A harness that offered it to the truncated arm
// would measure a product that does not exist - and its prompt budget with
// it, since the rules the prompt carries are per tool.
func TestTheArmsToolTableFollowsTheProductRegistration(t *testing.T) {
	offered := []string{"read_history_message", "browse_summary_segments"}

	on := armToolTable(LayeredArmOn, offered)
	if !reflect.DeepEqual(on, []string{"read_history_message", "browse_summary_segments"}) {
		t.Fatalf("the layered arm's table = %v", on)
	}
	off := armToolTable(LayeredArmTruncated, offered)
	if !reflect.DeepEqual(off, []string{"read_history_message"}) {
		t.Fatalf("the truncated arm's table = %v; the browse tool is registered only in the applied mode", off)
	}
}
