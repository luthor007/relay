import Foundation
import XCTest
@testable import RelayKit

/// `docs/SYSTEM.md` §6.1 is a contract two languages implement. The vector below
/// was produced by `glasses/bridge/src/relayd.ts`, which is the reference; a
/// disagreement here means every connection from this platform is refused with
/// an error that says nothing about why.
final class LinkAuthTests: XCTestCase {

    private let credential = DeviceCredential(
        deviceId: "phone-a",
        boxId: "box-1",
        deviceToken: "1122334455667788",
        signingKey: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
    )
    private let nonce = Data((0 ..< 16).map { UInt8($0) })

    func testAuthHeaderMatchesTheTypeScriptByteForByte() {
        let header = LinkAuth.header(credential, timestampMs: 1_700_000_000_000, nonce: nonce)
        XCTAssertEqual(
            header,
            "relay.auth.phone-a.1122334455667788.1700000000000."
                + "000102030405060708090a0b0c0d0e0f."
                + "7bc8bbc16688d770bec4e4bf9a7211a155faaaa7be8a4dbc8f25cd72720107a4"
        )
    }

    func testTheSigningKeyIsHexDecodedNotUsedAsAString() {
        // The connector signs with the key's *characters*; the link signs with
        // its *bytes*. Getting this backwards produces a header that looks
        // entirely plausible and never verifies.
        let asCharacters = ConnectorClient.sign(
            signingKey: credential.signingKey,
            method: "POST",
            path: "/v1/pair",
            bodyHash: "",
            timestampMs: 1_700_000_000_000
        )
        let header = LinkAuth.header(credential, timestampMs: 1_700_000_000_000, nonce: nonce)
        XCTAssertFalse(header.contains(asCharacters))
    }

    func testTheTimestampIsBoundIntoTheSignature() {
        let a = LinkAuth.header(credential, timestampMs: 1_700_000_000_000, nonce: nonce)
        let b = LinkAuth.header(credential, timestampMs: 1_700_000_000_001, nonce: nonce)
        XCTAssertNotEqual(a, b, "a captured header must not be replayable outside its window")
    }

    func testTheNonceIsBoundIntoTheSignature() {
        let other = Data(repeating: 0xAB, count: 16)
        XCTAssertNotEqual(
            LinkAuth.header(credential, timestampMs: 1_700_000_000_000, nonce: nonce),
            LinkAuth.header(credential, timestampMs: 1_700_000_000_000, nonce: other)
        )
    }

    func testDeviceIdIsPercentEncodedWithTheJavaScriptSet() {
        // `CharacterSet.urlQueryAllowed` would let `&`, `=` and `+` through, and
        // a dot-separated header cannot survive any of them.
        XCTAssertEqual(LinkAuth.percentEncoded("a.b"), "a.b")
        XCTAssertEqual(LinkAuth.percentEncoded("a/b"), "a%2Fb")
        XCTAssertEqual(LinkAuth.percentEncoded("a b"), "a%20b")
        XCTAssertEqual(LinkAuth.percentEncoded("a=b&c"), "a%3Db%26c")
        XCTAssertEqual(LinkAuth.percentEncoded("~_-.!*'()"), "~_-.!*'()")
    }
}

final class EnvelopeTests: XCTestCase {

    func testAValidEnvelopeRoundTrips() throws {
        let envelope = RelaydEnvelope(
            id: "abc",
            type: PhoneMessage.utterance.rawValue,
            at: 1_700_000_000_000,
            payload: .object(["text": .string("what did I say about the fixture?")])
        )
        let parsed = try Envelopes.parse(Envelopes.serialise(envelope))
        XCTAssertEqual(parsed, envelope)
    }

    func testAnEnvelopeWithTheWrongVersionIsRejectedAsAVersionMismatch() {
        let text = #"{"v":2,"id":"a","type":"speak","at":1,"payload":null}"#
        XCTAssertThrowsError(try Envelopes.parse(text)) { error in
            XCTAssertEqual((error as? LinkError)?.code, .versionMismatch)
        }
    }

    func testAnEnvelopeMissingItsIdIsMalformedRatherThanHalfParsed() {
        let text = #"{"v":1,"type":"speak","at":1,"payload":null}"#
        XCTAssertThrowsError(try Envelopes.parse(text)) { error in
            XCTAssertEqual((error as? LinkError)?.code, .malformed)
        }
    }

    func testAnEmptyTypeIsMalformed() {
        let text = #"{"v":1,"id":"a","type":"","at":1,"payload":null}"#
        XCTAssertThrowsError(try Envelopes.parse(text))
    }

    func testTheVocabulariesAreExactlyTheOnesInSystemMd() {
        XCTAssertEqual(
            PhoneMessage.allCases.map(\.rawValue),
            ["utterance", "touch", "wear", "audio.chunk", "photo", "session.command",
             "consent.decision", "sync.offer"]
        )
        // All ten. The last four are the transport frames §6.1 added while
        // `relayd/internal/api` was written; each was dropped on the floor here
        // until it was listed, and a swallowed `notify` is the quiet-hours
        // behaviour failing with nothing in the log.
        XCTAssertEqual(
            ServerMessage.allCases.map(\.rawValue),
            ["speak", "ui.render", "session.list", "confirm.request",
             "connector.proposal", "digest", "ack", "error", "notify", "confirm.resolved"]
        )
        XCTAssertEqual(
            ServerMessage.allCases.filter(\.isProduct).map(\.rawValue),
            ["speak", "ui.render", "session.list", "confirm.request",
             "connector.proposal", "digest"]
        )
    }

    func testEnvelopeIdsAreRfc4122Version4() {
        let id = Envelopes.newId(countingRandom)
        XCTAssertEqual(id.count, 36)
        let parts = id.split(separator: "-")
        XCTAssertEqual(parts.map(\.count), [8, 4, 4, 4, 12])
        XCTAssertTrue(parts[2].hasPrefix("4"), "version nibble")
        XCTAssertTrue(["8", "9", "a", "b"].contains(String(parts[3].prefix(1))), "variant nibble")
    }

    func testBackoffMatchesTheTypeScript() {
        // Vectors from `backoffMs` in glasses/bridge/src/relayd.ts.
        XCTAssertEqual(backoffMs(attempt: 0, roll: 0), 250)
        XCTAssertEqual(backoffMs(attempt: 0, roll: 1), 500)
        XCTAssertEqual(backoffMs(attempt: 3, roll: 0), 2_000)
        XCTAssertEqual(backoffMs(attempt: 3, roll: 1), 4_000)
        XCTAssertEqual(backoffMs(attempt: 10, roll: 1), 30_000, "maxMs must be a real ceiling")
        XCTAssertEqual(
            backoffMs(attempt: 5, options: BackoffOptions(baseMs: 1_000, maxMs: 8_000, jitter: 0)),
            8_000
        )
    }

    func testJitterSubtractsSoTheCeilingHolds() {
        for attempt in 0 ... 12 {
            for roll in stride(from: 0.0, through: 1.0, by: 0.25) {
                XCTAssertLessThanOrEqual(backoffMs(attempt: attempt, roll: roll), 30_000)
            }
        }
    }
}

final class RelaydLinkTests: XCTestCase {

    private let credential = DeviceCredential(
        deviceId: "phone-a",
        boxId: "box-1",
        deviceToken: "1122334455667788",
        signingKey: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
    )

    private func makeLink(
        sealed: Bool = false,
        sealer: EnvelopeSealer? = nil,
        outboxLimit: Int = 1_000,
        observer: LinkObserver = LinkObserver()
    ) -> (RelaydLink, MockSocketFactory, TestClock) {
        let factory = MockSocketFactory()
        let clock = TestClock(startMs: 1_700_000_000_000)
        let link = RelaydLink(
            url: URL(string: "wss://relay.example/link")!,
            credential: credential,
            socketFactory: factory.make(),
            clock: clock,
            random: countingRandom,
            outboxLimit: outboxLimit,
            sealed: sealed,
            sealer: sealer,
            observer: observer
        )
        return (link, factory, clock)
    }

    func testCredentialsRideTheSubprotocolNotTheUrl() throws {
        let (link, factory, _) = makeLink()
        link.connect()

        let socket = try XCTUnwrap(factory.latest)
        XCTAssertEqual(socket.protocols.first, linkSubprotocol)
        XCTAssertNotNil(socket.authHeader)
        XCTAssertFalse(
            socket.url.absoluteString.contains(credential.deviceToken),
            "a bearer token in a query string ends up in proxy logs"
        )
    }

    func testEnvelopesQueueWhileTheLinkIsDownAndGoOutWhenItOpens() {
        let (link, factory, _) = makeLink()
        link.connect()

        link.send(.utterance, .object(["text": .string("hello")]))
        XCTAssertEqual(factory.latest?.sent.count, 0, "nothing may go out before the socket opens")
        XCTAssertEqual(link.pending, 1)

        factory.latest?.simulateOpen()
        XCTAssertEqual(factory.latest?.sent.count, 1)
        XCTAssertEqual(factory.latest?.sentEnvelopes.first?.type, "utterance")
    }

    func testAnAbnormalCloseReturnsInFlightWorkToTheHeadOfTheOutbox() {
        let redelivered = Locked([String]())
        let (link, factory, _) = makeLink(
            observer: LinkObserver(onRedelivered: { ids in redelivered.set(ids) })
        )
        link.connect()
        factory.latest?.simulateOpen()

        let first = link.send(.utterance, .string("one"))
        let second = link.send(.utterance, .string("two"))
        XCTAssertEqual(link.pending, 2, "sent is not the same as delivered")

        factory.latest?.simulateDrop()

        XCTAssertEqual(redelivered.get(), [first, second].compactMap { $0 })
        XCTAssertEqual(
            link.pendingIds, [first, second].compactMap { $0 },
            "order must be preserved — relayd segments by time, so a reordered "
                + "utterance lands in the wrong session"
        )
    }

    func testAnAckPrunesWhatIsHeld() {
        // `{ re, ok }` — relayd/internal/api/wire.go's `Ack`, one per accepted
        // frame. The batched `link.ack` this used to expect was a client-side
        // invention no daemon ever sent, so nothing was ever pruned.
        let acks = Locked([AckFrame]())
        let (link, factory, _) = makeLink(
            observer: LinkObserver(onAck: { ack in acks.mutate { $0.append(ack) } })
        )
        link.connect()
        factory.latest?.simulateOpen()

        let id = link.send(.utterance, .string("one"))
        XCTAssertEqual(link.pending, 1)

        factory.latest?.simulateMessage(
            #"{"v":1,"id":"ack-1","type":"ack","at":1,"payload":{"re":"\#(id ?? "")","ok":true}}"#
        )
        XCTAssertEqual(link.pending, 0)
        XCTAssertEqual(acks.get().map(\.pruned), [true])
        XCTAssertEqual(acks.get().first?.re, id)
    }

    func testANegativeAckKeepsTheEnvelopeHeld() {
        let (link, factory, _) = makeLink()
        link.connect()
        factory.latest?.simulateOpen()

        let id = link.send(.utterance, .string("one"))
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"ack-1","type":"ack","at":1,"payload":{"re":"\#(id ?? "")","ok":false}}"#
        )
        XCTAssertEqual(link.pending, 1, "a negative ack is not a delivery receipt")
    }

    func testEveryFrameTheDocListsReachesAListener() {
        // The finding, as a test: all ten of §6.1's server→phone frames must be
        // handled. Four of them used to fall through to `onUnknownType` and be
        // discarded — including `notify`, which `docs/ADAPTERS.md` §7 requires to
        // arrive without speech.
        let routed = Locked([String]())
        let unknown = Locked([String]())
        let (link, factory, _) = makeLink(observer: LinkObserver(
            onServerMessage: { type, _ in routed.mutate { $0.append(type.rawValue) } },
            onUnknownType: { envelope in unknown.mutate { $0.append(envelope.type) } }
        ))
        link.connect()
        factory.latest?.simulateOpen()

        for type in ServerMessage.allCases {
            factory.latest?.simulateMessage(
                #"{"v":1,"id":"s-\#(type.rawValue)","type":"\#(type.rawValue)","at":1,"payload":{}}"#
            )
        }

        XCTAssertEqual(routed.get(), ServerMessage.allCases.map(\.rawValue))
        XCTAssertEqual(unknown.get(), [], "nothing the doc lists may be treated as unknown")
    }

    func testAnErrorRetiresTheFrameItNamesInsteadOfRetryingForever() {
        // relayd's own M4 answer: capture is not built, so an `audio.chunk` comes
        // back `not_implemented` with the milestone attached. Holding it would
        // redeliver it on every reconnect for the life of the queue.
        let errors = Locked([ServerErrorFrame]())
        let (link, factory, _) = makeLink(
            observer: LinkObserver(onServerError: { frame in errors.mutate { $0.append(frame) } })
        )
        link.connect()
        factory.latest?.simulateOpen()

        let id = link.send(.audioChunk, .object(["seq": .number(1)]))
        XCTAssertEqual(link.pending, 1)

        factory.latest?.simulateMessage(
            #"{"v":1,"id":"e-1","type":"error","at":1,"payload":"#
                + #"{"re":"\#(id ?? "")","code":"not_implemented","#
                + #""message":"keep it on the device","milestone":"M4 — capture and memory"}}"#
        )

        XCTAssertEqual(link.pending, 0, "a refusal is an answer")
        XCTAssertEqual(errors.get().first?.code, ServerErrorCode.notImplemented)
        XCTAssertEqual(errors.get().first?.cancelled, true)
        XCTAssertEqual(errors.get().first?.milestone, "M4 — capture and memory")
    }

    func testAServerErrorIsNotTheLinksOwnError() {
        let linkErrors = Locked([LinkErrorCode]())
        let serverErrors = Locked([String]())
        let (link, factory, _) = makeLink(observer: LinkObserver(
            onServerError: { frame in serverErrors.mutate { $0.append(frame.code) } },
            onError: { error in linkErrors.mutate { $0.append(error.code) } }
        ))
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"e-1","type":"error","at":1,"payload":{"code":"no_such_session","message":"gone"}}"#
        )

        XCTAssertEqual(serverErrors.get(), [ServerErrorCode.noSuchSession])
        XCTAssertEqual(
            linkErrors.get(), [],
            "the daemon refusing an action is not the socket breaking; a UI that "
                + "conflates them retries the first forever"
        )
    }

    func testANotifyArrivesWithoutSpeechAndSilentStillMeansPresent() {
        let notifications = Locked([NotifyFrame]())
        let spoken = Locked([String]())
        let (link, factory, _) = makeLink(observer: LinkObserver(
            onServerMessage: { type, _ in
                if type == .speak { spoken.mutate { $0.append("speak") } }
            },
            onNotify: { note in notifications.mutate { $0.append(note) } }
        ))
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"n-1","type":"notify","at":1,"payload":"#
                + #"{"title":"done","body":"14 files","sessions":["s-1"],"silent":true}}"#
        )

        XCTAssertEqual(spoken.get(), [], "a notification is not an utterance")
        XCTAssertEqual(notifications.get().first?.title, "done")
        XCTAssertEqual(notifications.get().first?.sessions, ["s-1"])
        XCTAssertEqual(
            notifications.get().first?.silent, true,
            "quiet hours hold the speech and keep the notification"
        )
    }

    func testConfirmResolvedRetractsTheQuestionItNames() {
        let resolved = Locked([ConfirmResolvedFrame]())
        let (link, factory, _) = makeLink(
            observer: LinkObserver(onConfirmResolved: { frame in
                resolved.mutate { $0.append(frame) }
            })
        )
        link.connect()
        factory.latest?.simulateOpen()

        factory.latest?.simulateMessage(
            #"{"v":1,"id":"c-1","type":"confirm.request","at":1,"payload":{"action_id":"act-1"}}"#
        )
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"c-2","type":"confirm.request","at":1,"payload":{"action_id":"act-2"}}"#
        )
        XCTAssertEqual(link.outstandingConfirmations, ["act-1", "act-2"])

        factory.latest?.simulateMessage(
            #"{"v":1,"id":"r-1","type":"confirm.resolved","at":1,"#
                + #""payload":{"action_id":"act-1","reason":"answered in a terminal"}}"#
        )

        XCTAssertEqual(
            link.outstandingConfirmations, ["act-2"],
            "a ping that outlives its question wakes someone to approve what is already approved"
        )
        XCTAssertEqual(resolved.get().first?.wasOutstanding, true)
        XCTAssertEqual(resolved.get().first?.reason, "answered in a terminal")
    }

    func testANotifyBecomesANotificationThatQuietHoursCannotSilenceAway() {
        let loud = NotifyFrame(
            envelope: RelaydEnvelope(id: "e-1", type: "notify", at: 1),
            title: "auth-refactor finished",
            body: "14 files, tests green",
            sessions: ["s-1"],
            silent: false,
            ping: "p-1"
        )
        XCTAssertEqual(loud.presentation.playSound, true)
        XCTAssertEqual(loud.presentation.threadId, "s-1")

        let quiet = NotifyFrame(
            envelope: RelaydEnvelope(id: "e-2", type: "notify", at: 1),
            title: "auth-refactor finished",
            body: "14 files, tests green",
            sessions: [],
            silent: true,
            ping: "p-1"
        )
        XCTAssertEqual(quiet.presentation.playSound, false, "silent is soundless, not absent")
        XCTAssertEqual(quiet.presentation.body, "14 files, tests green")
        XCTAssertEqual(
            quiet.presentation.identifier, loud.presentation.identifier,
            "relayd re-pings the same question every two minutes; keyed by ping, the "
                + "re-ping replaces the banner instead of stacking a fifth one"
        )
    }

    func testANotifyWithNoTitleIsStillNameable() {
        let bare = NotifyFrame(
            envelope: RelaydEnvelope(id: "e-3", type: "notify", at: 1),
            title: "",
            body: "something happened",
            sessions: [],
            silent: false,
            ping: nil
        )
        XCTAssertEqual(bare.presentation.title, "Relay", "a nameless banner is worse than the app's name")
        XCTAssertEqual(bare.presentation.identifier, "e-3", "with no ping, the envelope id keys it")
        XCTAssertNil(bare.presentation.threadId)
    }

    func testAnsweringClosesTheQuestionWithoutWaitingForTheBox() {
        let (link, factory, _) = makeLink()
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"c-1","type":"confirm.request","at":1,"payload":{"action_id":"act-1"}}"#
        )
        XCTAssertEqual(link.outstandingConfirmations, ["act-1"])

        link.send(.consentDecision, .object(["action_id": .string("act-1"), "approved": .bool(true)]))
        XCTAssertEqual(link.outstandingConfirmations, [])
    }

    func testAFullOutboxRefusesTheNewestAndSaysSo() {
        let errors = Locked([LinkErrorCode]())
        let (link, factory, _) = makeLink(
            outboxLimit: 1,
            observer: LinkObserver(onError: { error in errors.mutate { $0.append(error.code) } })
        )
        link.connect()
        _ = factory

        XCTAssertNotNil(link.send(.utterance, .string("one")))
        XCTAssertNil(link.send(.utterance, .string("two")), "the newest is refused, not the oldest")
        XCTAssertEqual(errors.get(), [.outboxFull])
        XCTAssertEqual(link.pendingIds.count, 1)
    }

    func testAnUnknownInboundTypeIsSurfacedRatherThanFatal() {
        let unknown = Locked([String]())
        let seen = Locked([String]())
        let (link, factory, _) = makeLink(observer: LinkObserver(
            onMessage: { envelope in seen.mutate { $0.append(envelope.type) } },
            onUnknownType: { envelope in unknown.mutate { $0.append(envelope.type) } }
        ))
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateMessage(
            #"{"v":1,"id":"x","type":"weather.report","at":1,"payload":null}"#
        )

        XCTAssertEqual(unknown.get(), ["weather.report"])
        XCTAssertEqual(link.state, .open, "forward compatibility is not an error")
    }

    func testAMalformedFrameIsReportedAndTheLinkStaysOpen() {
        let errors = Locked([LinkErrorCode]())
        let (link, factory, _) = makeLink(
            observer: LinkObserver(onError: { error in errors.mutate { $0.append(error.code) } })
        )
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateMessage("this is not JSON")

        XCTAssertEqual(errors.get(), [.malformed])
        XCTAssertEqual(link.state, .open)
    }

    func testADeliberateCloseKeepsQueuedWorkAndDoesNotReconnect() {
        let (link, factory, _) = makeLink()
        link.connect()
        factory.latest?.simulateOpen()
        link.send(.utterance, .string("said out loud"))

        link.close()

        XCTAssertEqual(link.state, .closed)
        XCTAssertEqual(
            link.pending, 1,
            "the app being closed is not a reason to throw away something the user already said"
        )
        XCTAssertEqual(factory.sockets.count, 1, "a deliberate close must not schedule a retry")
    }

    func testADropSchedulesAReconnectAndWakeCollapsesTheBackoff() {
        let (link, factory, _) = makeLink()
        link.connect()
        factory.latest?.simulateOpen()
        factory.latest?.simulateDrop()

        XCTAssertEqual(link.state, .reconnecting)
        XCTAssertEqual(factory.sockets.count, 1, "the retry is waiting out its backoff")

        link.wake()

        XCTAssertEqual(link.state, .connecting)
        XCTAssertEqual(factory.sockets.count, 2, "wake must not wait out maxMs for no reason")
        XCTAssertEqual(link.attempt, 0)
    }

    func testRelayModeWithoutASealerRefusesToConnect() {
        let errors = Locked([LinkErrorCode]())
        let (link, factory, _) = makeLink(
            sealed: true,
            observer: LinkObserver(onError: { error in errors.mutate { $0.append(error.code) } })
        )
        link.connect()

        XCTAssertEqual(factory.sockets.count, 0, "no socket may open")
        XCTAssertEqual(errors.get(), [.sealFailed])
        XCTAssertEqual(
            link.state, .closed,
            "a missing feature that stops the app is a bug report; one that silently "
                + "sends an utterance in the clear through our own relay is a breach"
        )
    }
}
