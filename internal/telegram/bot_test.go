package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mtzanidakis/praktor/internal/agent"
	"github.com/mymmrac/telego"
)

func TestChunkMessage(t *testing.T) {
	// Short message
	chunks := chunkMessage("hello")
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}

	// Exact limit
	msg := make([]byte, 4096)
	for i := range msg {
		msg[i] = 'a'
	}
	chunks = chunkMessage(string(msg))
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for exact limit, got %d", len(chunks))
	}

	// Over limit
	msg = make([]byte, 8192)
	for i := range msg {
		msg[i] = 'a'
	}
	chunks = chunkMessage(string(msg))
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}

	// Split at newline
	msg = make([]byte, 5000)
	for i := range msg {
		msg[i] = 'a'
	}
	msg[3000] = '\n'
	chunks = chunkMessage(string(msg))
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks with newline split, got %d", len(chunks))
	}
	if len(chunks[0]) != 3001 { // Up to and including the newline
		t.Errorf("expected first chunk length 3001, got %d", len(chunks[0]))
	}
}

func TestEscapeMarkdownV2(t *testing.T) {
	tests := []struct {
		name     string
		in, want string
	}{
		{"plain text", "hello world", "hello world"},
		{"dots", "file.txt", "file\\.txt"},
		{"parens", "foo(bar)", "foo\\(bar\\)"},
		{"exclamation", "done!", "done\\!"},
		{"mixed special", "a.b(c)d!", "a\\.b\\(c\\)d\\!"},
		{"hash", "#tag", "\\#tag"},
		{"pipe", "a|b", "a\\|b"},
		{"brackets", "[test]", "\\[test\\]"},
		{"tilde", "~strike~", "\\~strike\\~"},
		{"plus minus equals", "a+b-c=d", "a\\+b\\-c\\=d"},
		{"braces", "{key}", "\\{key\\}"},
		{"backslash passthrough", `a\b`, `a\b`}, // \ is the escape char itself, not escaped
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeMarkdownV2(tt.in)
			if got != tt.want {
				t.Errorf("escapeMarkdownV2(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToTelegramMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		in, want string
	}{
		{"bold", "**bold**", "*bold*"},
		{"bold inline", "hello **world**!", "hello *world*\\!"},
		{"multiple bold", "**a** and **b**", "*a* and *b*"},
		{"no bold", "no bold here", "no bold here"},
		{"already single", "*already single*", "*already single*"},
		{"h1 header", "# Title", "*Title*"},
		{"h2 header", "## Section", "*Section*"},
		{"h3 header", "### Subsection", "*Subsection*"},
		{"header with bold", "## **Bold Title**", "**Bold Title**"},
		{"horizontal rule", "---", ""},
		{"long horizontal rule", "-----", ""},
		{"hr between text", "above\n---\nbelow", "above\n\nbelow"},
		{"bullet dash", "- item one\n- item two", "• item one\n• item two"},
		{"bullet asterisk", "* item one\n* item two", "• item one\n• item two"},
		{"indented bullet", "  - nested item", "  • nested item"},
		{"image embed", "![screenshot](https://example.com/img.png)", "[screenshot](https://example.com/img.png)"},
		{"image no alt", "![](https://example.com/img.png)", "[](https://example.com/img.png)"},
		{"code block protected", "```\n## header\n- bullet\n**bold**\n---\n```", "```\n## header\n- bullet\n**bold**\n---\n```"},
		{"mixed with code block", "## Title\n```\n## not a header\n```\n- bullet", "*Title*\n```\n## not a header\n```\n• bullet"},
		// MarkdownV2-specific
		{"special chars escaped", "file.txt (copy)", "file\\.txt \\(copy\\)"},
		{"link preserved", "[click here](https://example.com)", "[click here](https://example.com)"},
		{"link with special text", "[file.txt](https://example.com/a.txt)", "[file\\.txt](https://example.com/a.txt)"},
		{"inline code preserved", "run `ls -la` now", "run `ls -la` now"},
		{"bold with dots", "**file.txt**", "*file\\.txt*"},
		{"bullet with special", "- item (a) and b.c", "• item \\(a\\) and b\\.c"},
		{"exclamation in text", "done!", "done\\!"},
		{"single star bold", "*Total: 5 files*", "*Total: 5 files*"},
		{"single star bold with dots", "*file.txt*", "*file\\.txt*"},
		{"pre-escaped underscore", `photo\_2024.jpg`, `photo\_2024\.jpg`},
		{"pre-escaped tilde", `data\~backup.csv`, `data\~backup\.csv`},
		{"pre-escaped dot", `file\.txt`, `file\.txt`},
		{"italic preserved", "_secretary:_ hello", "_secretary:_ hello"},
		{"italic with dots", "_agent:_ file.txt done!", "_agent:_ file\\.txt done\\!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toTelegramMarkdown(tt.in)
			if got != tt.want {
				t.Errorf("toTelegramMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestConvertMarkdownTables(t *testing.T) {
	input := "Here is a table:\n| Name | Value |\n|---|---|\n| BTC | $67,671 |\n| ETH | $3,052 |\n\nEnd."
	got := convertMarkdownTables(input)

	// Should contain pre-formatted block
	if !strings.Contains(got, "```") {
		t.Errorf("expected pre-formatted block, got:\n%s", got)
	}
	// Should preserve surrounding text
	if !strings.Contains(got, "Here is a table:") || !strings.Contains(got, "End.") {
		t.Errorf("surrounding text lost, got:\n%s", got)
	}
	// Should not contain raw pipe table syntax
	if strings.Contains(got, "|---|") {
		t.Errorf("separator row not removed, got:\n%s", got)
	}
	// Should contain box-drawing separator
	if !strings.Contains(got, "─┼─") {
		t.Errorf("expected box-drawing separator, got:\n%s", got)
	}
}

func TestConvertMarkdownTablesUnicodeAndBold(t *testing.T) {
	input := "| Ζεύγος | Τιμή |\n|---|---|\n| **BTC** | $68,500 |\n| **ETH** | $2,055 |"
	got := convertMarkdownTables(input)

	// Bold markers should be stripped inside pre block
	if strings.Contains(got, "**") || strings.Contains(got, "*BTC*") {
		t.Errorf("bold markers not stripped, got:\n%s", got)
	}
	// Check alignment: all data rows should have the same rune width
	lines := strings.Split(got, "\n")
	var dataWidths []int
	for _, line := range lines {
		if strings.Contains(line, "│") && !strings.Contains(line, "┼") {
			dataWidths = append(dataWidths, utf8.RuneCountInString(line))
		}
	}
	for i := 1; i < len(dataWidths); i++ {
		if dataWidths[i] != dataWidths[0] {
			t.Errorf("misaligned rows: line 0 has %d runes, line %d has %d runes\n%s",
				dataWidths[0], i, dataWidths[i], got)
		}
	}
}

func TestConvertMarkdownTablesNoTable(t *testing.T) {
	input := "No tables here.\nJust text."
	got := convertMarkdownTables(input)
	if got != input {
		t.Errorf("expected unchanged text, got:\n%s", got)
	}
}

func TestExtractAttachment(t *testing.T) {
	tests := []struct {
		name     string
		msg      telego.Message
		wantNil  bool
		wantName string
		wantMime string
	}{
		{
			name:     "document",
			msg:      telego.Message{Document: &telego.Document{FileID: "doc1", FileName: "report.pdf", MimeType: "application/pdf"}},
			wantName: "report.pdf",
			wantMime: "application/pdf",
		},
		{
			name:     "document without name",
			msg:      telego.Message{Document: &telego.Document{FileID: "doc2"}},
			wantName: "document",
			wantMime: "application/octet-stream",
		},
		{
			name: "photo multiple sizes",
			msg: telego.Message{Photo: []telego.PhotoSize{
				{FileID: "small", Width: 90, Height: 90},
				{FileID: "medium", Width: 320, Height: 320},
				{FileID: "large", Width: 800, Height: 800},
			}},
			wantName: "photo.jpg",
			wantMime: "image/jpeg",
		},
		{
			name:     "audio with name",
			msg:      telego.Message{Audio: &telego.Audio{FileID: "aud1", FileName: "song.mp3", MimeType: "audio/mpeg"}},
			wantName: "song.mp3",
			wantMime: "audio/mpeg",
		},
		{
			name:     "audio without name",
			msg:      telego.Message{Audio: &telego.Audio{FileID: "aud2"}},
			wantName: "audio.mp3",
			wantMime: "audio/mpeg",
		},
		{
			name:     "video with name",
			msg:      telego.Message{Video: &telego.Video{FileID: "vid1", FileName: "clip.mp4", MimeType: "video/mp4"}},
			wantName: "clip.mp4",
			wantMime: "video/mp4",
		},
		{
			name:     "video without name",
			msg:      telego.Message{Video: &telego.Video{FileID: "vid2"}},
			wantName: "video.mp4",
			wantMime: "video/mp4",
		},
		{
			name:     "voice",
			msg:      telego.Message{Voice: &telego.Voice{FileID: "voice1", MimeType: "audio/ogg"}},
			wantName: "voice.ogg",
			wantMime: "audio/ogg",
		},
		{
			name:     "voice without mime",
			msg:      telego.Message{Voice: &telego.Voice{FileID: "voice2"}},
			wantName: "voice.ogg",
			wantMime: "audio/ogg",
		},
		{
			name:     "video note",
			msg:      telego.Message{VideoNote: &telego.VideoNote{FileID: "vn1"}},
			wantName: "videonote.mp4",
			wantMime: "video/mp4",
		},
		{
			name:     "animation with name",
			msg:      telego.Message{Animation: &telego.Animation{FileID: "anim1", FileName: "funny.gif", MimeType: "video/mp4"}},
			wantName: "funny.gif",
			wantMime: "video/mp4",
		},
		{
			name:     "animation without name",
			msg:      telego.Message{Animation: &telego.Animation{FileID: "anim2"}},
			wantName: "animation.mp4",
			wantMime: "video/mp4",
		},
		{
			name:    "no attachment",
			msg:     telego.Message{Text: "just text"},
			wantNil: true,
		},
		{
			name:    "empty message",
			msg:     telego.Message{},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAttachment(tt.msg)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected attachment, got nil")
			}
			if got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
			if got.MimeType != tt.wantMime {
				t.Errorf("mime = %q, want %q", got.MimeType, tt.wantMime)
			}
		})
	}

	// Verify photo uses largest size
	msg := telego.Message{Photo: []telego.PhotoSize{
		{FileID: "small", Width: 90},
		{FileID: "large", Width: 800},
	}}
	got := extractAttachment(msg)
	if got.FileID != "large" {
		t.Errorf("expected largest photo (FileID=large), got %q", got.FileID)
	}
}

func TestIsTelegramReply(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"telegram message", map[string]string{"sender": "user:1", "chat_id": "1"}, true},
		{"scheduled task", map[string]string{"sender": "scheduler"}, true},
		{"no meta", nil, true},
		{"chat api", map[string]string{"sender": "user:api", agent.MetaChannel: agent.ChannelAPI}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTelegramReply(c.meta); got != c.want {
				t.Errorf("isTelegramReply() = %v, want %v", got, c.want)
			}
		})
	}
}
