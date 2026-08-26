-- +goose Up
-- UNIQUE(user_id, claim_date) 是防連點的最終保證:讓資料庫擋,不靠應用層(grill Q7)
CREATE TABLE platform.daily_claims (
  user_id    BIGINT      NOT NULL REFERENCES platform.users(id),
  claim_date DATE        NOT NULL,              -- 以使用者當時時區計算的當地日期
  claimed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  timezone   TEXT        NOT NULL,              -- 當下時區快照,供稽核重算
  streak     INT         NOT NULL,
  amount     BIGINT      NOT NULL,
  source     TEXT        NOT NULL DEFAULT 'normal',  -- normal | makeup(補簽卡未來用)
  PRIMARY KEY (user_id, claim_date)
);

-- streak 存欄位不現算:回推在跨月/補簽時會出微妙錯誤(grill Q7)
CREATE TABLE platform.user_daily_state (
  user_id         BIGINT PRIMARY KEY REFERENCES platform.users(id),
  current_streak  INT  NOT NULL DEFAULT 0,
  best_streak     INT  NOT NULL DEFAULT 0,
  last_claim_date DATE,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE platform.user_daily_state;
DROP TABLE platform.daily_claims;
