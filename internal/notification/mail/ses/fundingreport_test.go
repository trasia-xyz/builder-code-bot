package ses

import (
	"context"
	"testing"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification/fundingreport"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestNotifierSendsConciseFundingSubjectWithStatusAndPrefix(t *testing.T) {
	t.Parallel()
	fake := &fakeSES{}
	notifier, err := NewNotifier(fake, Options{
		Source: "alerts@example.com", To: []string{"ops@example.com"}, SubjectPrefix: " [prod] ",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := fundingreport.Render(funding.RunReport{
		Status: funding.ReportStatusSuccess, RecordsRead: true, RecordCount: 42,
		PayoutTotal: "123.456789",
	})
	if err := notifier.Notify(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(fake.input.Message.Subject.Data); got != "🟢 [prod] Payout 123.46 USDC" {
		t.Fatalf("sent subject = %q", got)
	}
	if got := aws.ToString(fake.input.Message.Body.Html.Data); got != message.HTMLBody {
		t.Fatal("SES changed the rendered funding report body")
	}
}
