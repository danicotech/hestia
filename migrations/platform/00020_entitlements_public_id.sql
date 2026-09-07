-- +goose Up
-- 2026-09-14 定案:entitlements 補 public_id(全系統慣例,08 設計時的疏漏)
-- 對外 API 定址用它(/entitlements/{public_id}/refund);表目前零資料,免回填
ALTER TABLE platform.entitlements
  ADD COLUMN public_id TEXT NOT NULL UNIQUE;

-- +goose Down
ALTER TABLE platform.entitlements DROP COLUMN public_id;
