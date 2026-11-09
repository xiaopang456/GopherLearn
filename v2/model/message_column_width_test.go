package model

import (
	"reflect"
	"regexp"
	"testing"
)

// declaredWidth reads the character width a column was declared with. A column
// without an explicit width is not counted: this check is about the two columns
// that hold the same value, and both of them name their width.
func declaredWidth(t *testing.T, structPointer any, fieldName string) int {
	t.Helper()
	field, ok := reflect.TypeOf(structPointer).Elem().FieldByName(fieldName)
	if !ok {
		t.Fatalf("%T has no field %s", structPointer, fieldName)
	}
	match := regexp.MustCompile(`type:varchar\((\d+)\)`).FindStringSubmatch(field.Tag.Get("gorm"))
	if match == nil {
		t.Fatalf("%s.%s declares no varchar width: %q", reflect.TypeOf(structPointer).Elem().Name(), fieldName, field.Tag.Get("gorm"))
	}
	width := 0
	for _, digit := range match[1] {
		width = width*10 + int(digit-'0')
	}
	return width
}

// TestMessageUserNameColumnIsAsWideAsTheUsernameItStores is the invariant behind
// the column width: a message row stores the session owner's username, and the
// username column in the users table is the widest that value can be. A message
// column narrower than that refuses a legal username - MySQL either refuses the
// row or truncates it, and a truncated name is a message attributed to nobody.
func TestMessageUserNameColumnIsAsWideAsTheUsernameItStores(t *testing.T) {
	username := declaredWidth(t, &User{}, "Username")
	message := declaredWidth(t, &Message{}, "UserName")
	if message < username {
		t.Fatalf("messages.user_name is varchar(%d) but users.username is varchar(%d): a legal username does not fit", message, username)
	}
}
