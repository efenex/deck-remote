# Real-iPhone acceptance

Run this manually on a physical iPhone against an operator-provided disposable
acceptance instance with trusted HTTPS and synthetic sessions. This document is
a checklist, not evidence of a device run. Do not use production sessions or
execute deployment/merge commands as acceptance actions. vphone is out of scope:
do not weaken SIP or AMFI. Desktop [WebKit e2e](../e2e/README.md) is a separate gate.

## Record the run

- Date/time (UTC), tester, iPhone model, iOS version/build: __________
- Acceptance origin, app/server commit and displayed web version: __________
- Network (Wi-Fi/cellular), Focus mode, notification settings: __________
- For each checkbox record PASS / FAIL / BLOCKED, actual result and evidence ID.
  Keep screenshots free of QR codes, tokens, enrol codes and personal messages.
- Use a dedicated test device identity and synthetic target. Arrange a harmless
  acceptance command and a test notification with the operator before starting.
  A missing backend capability is BLOCKED, not PASS.

## PWA install, launch and recovery

- [ ] Open the acceptance HTTPS origin in Safari. Confirm the expected app and
  version, with no certificate warning or unexpected redirect.
- [ ] Share → Add to Home Screen. Check the icon/name, launch from the icon and
  verify standalone display, readable safe areas, and reachable bottom tabs.
- [ ] Pair the installed app using its **Scan pairing QR** flow and a fresh test
  QR from the operator. Allow camera access; confirm the intended device identity.
  Cancel/deny camera once and verify retry remains possible. A used QR must fail.
- [ ] Close and reopen the installed app; pairing and selected profile persist.
  Do not assume the Safari tab and installed app share pairing/storage.
- [ ] Background, lock/unlock, and resume with a sheet open. The list and stream
  reconnect, stale data is labelled until refreshed, and navigation remains usable.
- [ ] Briefly disable networking, refresh, then restore it. Refresh terminates
  with useful failure feedback; resume/Resync recovers without losing pairing or
  saved drafts. No message is silently sent twice.
- [ ] With an operator-staged shell update, check update/Resync behavior: unsaved
  input defers automatic reload, and an accepted refresh shows the expected version.

## Composer and native keyboard

Repeat core typing/send checks in Safari and the installed PWA; record both results.

- [ ] Open a synthetic session, focus the composer, type multiline text and scroll
  the transcript. Keyboard, caret, photo controls and Send remain reachable.
- [ ] Tap Send once with the keyboard open: one request/one bubble, input clears
  after acceptance, typing focus remains and the button does not move mid-tap.
- [ ] Press then drag away from Send: no send and draft retained. Test dictation,
  emoji, selection/paste and hardware-keyboard/accessibility activation if available.
- [ ] Dismiss the keyboard with Done, tap a photo control, and close/reopen the
  sheet. No stranded keyboard-height layout, covered tabs or inert background.
- [ ] Pick two photos from the native library; preview/remove one. Cancel the
  picker and return. Test camera capture. Selection alone must not send.
- [ ] Send a caption/photo to the synthetic sink; verify one attachment turn and
  caption ordering after reopen. Induce an acceptance-server upload failure:
  attachment and caption stay available for an explicit retry.
- [ ] In Tower, type a draft and refresh: text, focus and caret survive. Exercise
  reply-confirm cancel, success and refusal; after keyboard dismissal all tabs
  remain reachable. A closed ask retains its draft for copying.

## Passkey / Face ID

- [ ] After pairing, choose **Set up Face ID**; confirm the system prompt names
  the intended acceptance origin and the app lists the enrolled passkey.
- [ ] On a separate fresh test identity, exercise Settings → Passkey → **Enrol
  with Face ID** using a fresh operator-provided enrol code. Cancel once, then
  retry. Used/expired codes fail clearly; never put codes in evidence.
- [ ] Open the prearranged harmless command's confirmation sheet. Inspect the
  exact command, working directory and timeout before continuing. Cancel Face ID:
  the synthetic runner records zero executions and the sheet remains recoverable.
- [ ] Retry and complete Face ID: exactly one synthetic execution, with passkey
  authentication recorded by the acceptance backend. Reopening does not rerun it.
- [ ] A laptop-only command has no executable Run control. An unconfigured origin
  or identity without the required capability cannot bypass authentication.
- [ ] Revoke only the dedicated test credential/device at cleanup; subsequent
  authentication fails clearly. Restore the tester's original state if needed.

## Push and notification navigation

iOS Web Push requires an installed Home Screen web app on iOS 16.4 or later,
and permission must be requested from a user gesture. See
[WebKit's platform requirements](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/)
and [Apple's Web Push guide](https://developer.apple.com/documentation/usernotifications/sending-web-push-notifications-in-web-apps-and-browsers).

- [ ] In the installed PWA, Settings prerequisites show HTTPS, installation and
  push support. Tap **Enable notifications**, accept the OS prompt and verify
  subscribed status. Reopening retains the subscription without another prompt.
- [ ] On a fresh test install, deny permission. The UI explains recovery through
  iOS notification settings, and the rest of the app remains usable. Re-enable
  permission and verify subscription succeeds.
- [ ] Tap **Send test push**, then background/lock the phone. Verify an actual
  notification arrives; an HTTP delivery receipt alone is insufficient. Record
  observed delay and Focus/quiet-hours settings without treating delay as success.
- [ ] Have the acceptance backend emit synthetic approval/question and finished
  notifications while locked. Tap each: approval/question opens the intended
  session, finished opens the intended inbox entry, with no duplicate action.
- [ ] Repeat notification tap with the PWA already open on another sheet and
  after closing it. Both warm and cold launches reach the correct target.
- [ ] Test notification preferences and quiet hours with synthetic events, then
  restore them. Confirm re-enabling does not create duplicate alerts.

## Sign-off

- PWA: ____  Composer: ____  Passkey: ____  Push: ____
- Findings (steps, expected/actual, device/build, evidence): __________
- Cleanup of test subscriptions, identities and synthetic data: __________
- Operator acceptance / remaining blockers: __________

All four groups need device evidence before claiming iPhone acceptance. Keep
product failures as findings; a green desktop WebKit run cannot waive them.
