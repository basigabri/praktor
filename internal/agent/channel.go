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
// found: the agent's last message meta, which is only a guess. When that last
// message came from the chat API, the orphan is most likely that API run's
// reply (a duplicate, or a run the orchestrator no longer tracks). It is
// tagged as API without a request_id, so it answers no request and Telegram
// skips it instead of posting it to the chat that last talked to the agent.
func fallbackReplyMeta(last map[string]string) map[string]string {
	if last[MetaChannel] == ChannelAPI {
		return map[string]string{MetaChannel: ChannelAPI}
	}
	return last
}
