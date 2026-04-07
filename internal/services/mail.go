package services

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/gomail.v2"
)

type MailService struct {
	host     string
	port     int
	user     string
	password string
	from     string
}

func NewMailService(host, port, user, password string) *MailService {
	p, _ := strconv.Atoi(port)
	return &MailService{host: host, port: p, user: user, password: password, from: user}
}

type WeeklyReport struct {
	PagesOptimized   int
	AvgCTRBefore     float64
	AvgCTRAfter      float64
	KeywordsTop5     int
	TopWins          []ReportWin
	PendingApprovals int
	FlatChanges      []string
}

type ReportWin struct {
	Page     string
	CTRBefore float64
	CTRAfter  float64
}

func (m *MailService) SendWeeklyReport(to string, report *WeeklyReport) error {
	subject := fmt.Sprintf("91Astrology SEO Report — Week of %s", time.Now().Format("Jan 02"))
	body := m.buildReportHTML(report)

	msg := gomail.NewMessage()
	msg.SetHeader("From", m.from)
	msg.SetHeader("To", to)
	msg.SetHeader("Subject", subject)
	msg.SetBody("text/html", body)

	d := gomail.NewDialer(m.host, m.port, m.user, m.password)
	return d.DialAndSend(msg)
}

func (m *MailService) SendApprovalNotification(to string, pendingCount int) error {
	msg := gomail.NewMessage()
	msg.SetHeader("From", m.from)
	msg.SetHeader("To", to)
	msg.SetHeader("Subject", fmt.Sprintf("91Astro SEO: %d suggestions awaiting approval", pendingCount))
	msg.SetBody("text/html", fmt.Sprintf(`
		<p>You have <strong>%d SEO suggestions</strong> waiting for your review.</p>
		<p><a href="https://91astrology.com/admin/seo">Review in dashboard →</a></p>
	`, pendingCount))

	d := gomail.NewDialer(m.host, m.port, m.user, m.password)
	return d.DialAndSend(msg)
}

func (m *MailService) SendRollbackAlert(to, featureTitle string, bounceRateDelta float64) error {
	msg := gomail.NewMessage()
	msg.SetHeader("From", m.from)
	msg.SetHeader("To", to)
	msg.SetHeader("Subject", fmt.Sprintf("AUTO-ROLLBACK: %s", featureTitle))
	msg.SetBody("text/html", fmt.Sprintf(`
		<p>The SEO agent auto-rolled back: <strong>%s</strong></p>
		<p>Reason: Bounce rate increased by <strong>%.1f%%</strong> (above threshold)</p>
		<p><a href="https://91astrology.com/admin/seo/features">View in dashboard →</a></p>
	`, featureTitle, bounceRateDelta))

	d := gomail.NewDialer(m.host, m.port, m.user, m.password)
	return d.DialAndSend(msg)
}

func (m *MailService) buildReportHTML(r *WeeklyReport) string {
	ctrImprovement := r.AvgCTRAfter - r.AvgCTRBefore
	ctrSign := "+"
	if ctrImprovement < 0 {
		ctrSign = ""
	}

	var wins strings.Builder
	for _, w := range r.TopWins {
		wins.WriteString(fmt.Sprintf(
			"<tr><td>%s</td><td>%.1f%% → %.1f%%</td><td style='color:green'>+%.1f%%</td></tr>",
			w.Page, w.CTRBefore, w.CTRAfter, w.CTRAfter-w.CTRBefore,
		))
	}

	return fmt.Sprintf(`
<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:600px;margin:0 auto;padding:20px">
  <h2>91Astrology SEO Weekly Report</h2>
  <p style="color:#666">Week of %s</p>

  <h3>Highlights</h3>
  <ul>
    <li><strong>%d</strong> pages optimized</li>
    <li>Avg CTR: <strong>%.1f%% → %.1f%%</strong> (%s%.1f%%)</li>
    <li><strong>%d</strong> keywords entered top 5</li>
  </ul>

  <h3>Top Wins</h3>
  <table width="100%%" style="border-collapse:collapse">
    <tr style="background:#f5f5f5">
      <th align="left">Page</th><th align="left">CTR</th><th align="left">Delta</th>
    </tr>
    %s
  </table>

  <h3>Pending Approval</h3>
  <p><strong>%d</strong> suggestions waiting review.
  <a href="https://91astrology.com/admin/seo">Review →</a></p>
</body>
</html>`,
		time.Now().Format("Jan 02, 2006"),
		r.PagesOptimized,
		r.AvgCTRBefore, r.AvgCTRAfter, ctrSign, ctrImprovement,
		r.KeywordsTop5,
		wins.String(),
		r.PendingApprovals,
	)
}
