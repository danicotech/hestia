// Package migrations 內嵌 SQL migration,讓測試與正式 binary 都能自帶 schema。
package migrations

import "embed"

// Platform 是 platform schema 的 goose migration 檔案。
//
//go:embed platform/*.sql
var Platform embed.FS

// Activity 是 activity schema 的 goose migration 檔案。
//
// 分開內嵌不是為了現在 —— 是為了 internal/core/activity 抽成 themis 的那天:
// 到時候這個變數、對應目錄與它自己的版本表整包搬走,platform 一行都不用動。
//
//go:embed activity/*.sql
var Activity embed.FS
