-- +goose Up
-- 本地憑證:讓平台帳號有第二種登入方式(provider='local')。
--
-- # 為什麼需要
--
-- 裁判的每一個動作(評段、抽籤、判勝負、發獎)都必須記在平台帳號上 ——
-- admin_audit_logs.actor_user_id 是 NOT NULL REFERENCES platform.users(id),
-- 而權限走 user_roles → judge 角色。所以「辦賽事的人不想接 Discord」這件事
-- 不能靠繞過平台帳號來解,只能給平台帳號第二種登入方式。
--
-- identities.provider 的欄位註解本來就寫著 discord | twitch | youtube | local:
-- local 是一開始就預留好的,只缺一個存密碼的地方。
--
-- # 為什麼不是新開一張表
--
-- 「一個使用者的一種登入方式」已經有權威位置了,就是 platform.identities ——
-- 它的 (provider, provider_user_id) 唯一鍵、user_id 外鍵、linked_at 全部
-- 原樣適用於本地帳號。另開一張 local_credentials 會讓「這個人有哪些登入方式」
-- 這個問題有兩個要 UNION 才答得完的來源,而 session 簽發、軟刪除檢查、
-- 帳號列舉防護這幾條路徑都得各寫兩份(專案鐵則 9)。
--
-- # 為什麼可為 NULL
--
-- OAuth 來的身分沒有密碼,那是正常狀態不是缺漏:Discord 綁定的憑證是
-- access_token_enc / refresh_token_enc,secret_hash 對它們沒有意義。
-- 硬給一個 NOT NULL DEFAULT '' 反而會讓「空字串算不算有效密碼」
-- 變成一條要靠應用層記得的規則。
--
-- 值的格式與選手通行碼共用同一套實作(internal/shared/secret):
--   pbkdf2-sha256$<迭代數>$<base64 鹽>$<base64 金鑰>
-- 迭代數寫在字串裡,所以日後調高成本不必把既有帳號作廢。
ALTER TABLE platform.identities ADD COLUMN secret_hash TEXT;

COMMENT ON COLUMN platform.identities.secret_hash IS
  '本地登入的通行碼雜湊(PBKDF2-HMAC-SHA256)。只有 provider=''local'' 會有值;OAuth 身分一律 NULL。';

-- +goose Down
ALTER TABLE platform.identities DROP COLUMN secret_hash;
