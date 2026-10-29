// Command schemainit creates the tables this application needs.
//
// The integrated entry refuses to change a schema at startup, so a fresh
// installation needs one explicit step: run this once against the database
// named in the configuration file, then start the entry.
//
// The migration is additive: it creates missing tables, columns and indexes and
// never drops or rewrites existing rows, so running it against a populated
// database keeps the data.
//
// The reranker is out of scope here: this tool reads no rerank credential and
// validates no rerank setting, so a deployment that enables the reranker can
// still create its schema.
//
// Usage:
//
//	go run ./cmd/schemainit -config ../config.integrated.toml.local
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"GopherAI/common/mysql"
	"GopherAI/config"
)

func main() {
	path := flag.String("config", "", "required explicit configuration file")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: schemainit -config <configuration file>")
		os.Exit(2)
	}
	loaded, err := config.LoadExplicitForSchema(*path)
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}
	if err := config.Use(loaded); err != nil {
		log.Fatalf("apply configuration: %v", err)
	}
	db, err := mysql.OpenConfigured()
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("database handle: %v", err)
	}
	defer sqlDB.Close()
	if err := mysql.InitializeSchema(db); err != nil {
		log.Fatalf("initialize schema: %v", err)
	}
	log.Printf("schema initialized in %s on %s:%d", loaded.MysqlDatabaseName, loaded.MysqlHost, loaded.MysqlPort)
}
