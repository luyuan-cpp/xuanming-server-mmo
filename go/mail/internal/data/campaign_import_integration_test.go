package data

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	mailpb "proto/mail"
)

// 活动名单导入改为「已导入数 = 各片 recipient_count 之和」与「整片一条多行 INSERT」之后(缺口清单第 27 条),
// 口径必须与以前逐人先查后插相同:有人已在别的片里 → 整片冲突、什么都不写;超出名单总数 → 参数无效;片重放照旧。
func TestTiDBCampaignImportCountsByShardAndRejectsCrossShardDuplicates(t *testing.T) {
	ctx, db, r := mailFixture(t)
	manifest := sha256.Sum256([]byte("campaign-import-shards"))
	request := &mailpb.SendMailRequest{SenderKind: 1, RequestId: 200, Title: "活动奖", Body: "多片名单", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 5}}},
		CampaignId: 21, ManifestGeneration: 1, ExpectedRecipientCount: 3, ExpectedShardCount: 3, ManifestHash: hex.EncodeToString(manifest[:])}
	if _, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, request); err != nil {
		t.Fatal(err)
	}
	importShard := func(index uint32, ids ...uint64) (*mailpb.ImportMailRecipientsResponse, error) {
		return r.Import(ctx, "admin", &mailpb.ImportMailRecipientsRequest{SenderKind: 1, CampaignId: 21, ManifestGeneration: 1, ShardIndex: index, PlayerIds: ids})
	}
	recipients := func() int {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_campaign_recipient WHERE campaign_id=21").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if out, err := importShard(0, 1, 2); err != nil || out.ImportedCount != 2 {
		t.Fatalf("shard 0: %+v %v", out, err)
	}
	if _, err := importShard(1, 2, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("player 2 is already in shard 0; shard 1 must conflict, got %v", err)
	}
	if n := recipients(); n != 2 {
		t.Fatalf("a conflicting shard must write nothing: %d recipients, want 2", n)
	}
	if out, err := importShard(1, 3); err != nil || out.ImportedCount != 1 {
		t.Fatalf("shard 1 after the conflict: %+v %v", out, err)
	}
	if out, err := importShard(1, 3); err != nil || !out.Duplicated || out.ImportedCount != 1 {
		t.Fatalf("shard 1 replay: %+v %v", out, err)
	}
	if _, err := importShard(2, 4); !errors.Is(err, ErrInvalid) {
		t.Fatalf("3 already imported of 3 expected; one more must be invalid, got %v", err)
	}
	if n := recipients(); n != 3 {
		t.Fatalf("recipients = %d, want 3", n)
	}
}
