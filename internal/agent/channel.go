package agent

// Message meta that tags where an incoming message came from. Output
// listeners receive the meta of the message being answered, so they can
// tell which replies are theirs to deliver.
const (
	// MetaChannel is the meta key holding the message's origin channel.
	MetaChannel = "channel"
	// ChannelAPI marks messages sent through the HTTP chat API. Their replies
	// go back in the HTTP response, never to Telegram.
	ChannelAPI = "api"
)

// fallbackReplyMeta is the meta used for a reply whose own message can't be
// found: the agent's last message meta, which is only a guess. A guess must
// never answer a specific chat API request, so API meta is dropped and the
// reply goes to the listeners' own fallbacks (Telegram's last chat).
func fallbackReplyMeta(last map[string]string) map[string]string {
	if last[MetaChannel] == ChannelAPI {
		return nil
	}
	return last
}
