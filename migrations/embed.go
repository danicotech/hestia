// Package migrations 內嵌 SQL migration,讓測試與正式 binary 都能自帶 schema。
package migrations

import "embed"

// Platform 是 platform schema 的 goose migration 檔案。
//
//go:embed platform/*.sql
var Platform embed.FS
