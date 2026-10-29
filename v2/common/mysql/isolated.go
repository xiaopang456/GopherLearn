package mysql

import "gorm.io/gorm"

// InitializeSchema exposes the original initialization sequence for an explicit
// isolated database. The entry never calls it unless migrate is explicitly true.
func InitializeSchema(db *gorm.DB) error { return migrate(db) }
