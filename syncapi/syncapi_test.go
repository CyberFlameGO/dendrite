package syncapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	keyapi "github.com/matrix-org/dendrite/keyserver/api"
	"github.com/matrix-org/dendrite/roomserver/api"
	rsapi "github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/setup/base"
	"github.com/matrix-org/dendrite/setup/jetstream"
	"github.com/matrix-org/dendrite/syncapi/types"
	"github.com/matrix-org/dendrite/test"
	"github.com/matrix-org/dendrite/test/testrig"
	userapi "github.com/matrix-org/dendrite/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/nats-io/nats.go"
	"github.com/tidwall/gjson"
)

type syncRoomserverAPI struct {
	rsapi.SyncRoomserverAPI
	rooms []*test.Room
}

func (s *syncRoomserverAPI) QueryLatestEventsAndState(ctx context.Context, req *rsapi.QueryLatestEventsAndStateRequest, res *rsapi.QueryLatestEventsAndStateResponse) error {
	var room *test.Room
	for _, r := range s.rooms {
		if r.ID == req.RoomID {
			room = r
			break
		}
	}
	if room == nil {
		res.RoomExists = false
		return nil
	}
	res.RoomVersion = room.Version
	return nil // TODO: return state
}

func (s *syncRoomserverAPI) QuerySharedUsers(ctx context.Context, req *rsapi.QuerySharedUsersRequest, res *rsapi.QuerySharedUsersResponse) error {
	res.UserIDsToCount = make(map[string]int)
	return nil
}
func (s *syncRoomserverAPI) QueryBulkStateContent(ctx context.Context, req *rsapi.QueryBulkStateContentRequest, res *rsapi.QueryBulkStateContentResponse) error {
	return nil
}

func (s *syncRoomserverAPI) QueryMembershipForUser(ctx context.Context, req *rsapi.QueryMembershipForUserRequest, res *rsapi.QueryMembershipForUserResponse) error {
	res.IsRoomForgotten = false
	res.RoomExists = true
	return nil
}

type syncUserAPI struct {
	userapi.SyncUserAPI
	accounts []userapi.Device
}

func (s *syncUserAPI) QueryAccessToken(ctx context.Context, req *userapi.QueryAccessTokenRequest, res *userapi.QueryAccessTokenResponse) error {
	for _, acc := range s.accounts {
		if acc.AccessToken == req.AccessToken {
			res.Device = &acc
			return nil
		}
	}
	res.Err = "unknown user"
	return nil
}

func (s *syncUserAPI) PerformLastSeenUpdate(ctx context.Context, req *userapi.PerformLastSeenUpdateRequest, res *userapi.PerformLastSeenUpdateResponse) error {
	return nil
}

type syncKeyAPI struct {
	keyapi.SyncKeyAPI
}

func (s *syncKeyAPI) QueryKeyChanges(ctx context.Context, req *keyapi.QueryKeyChangesRequest, res *keyapi.QueryKeyChangesResponse) {
}
func (s *syncKeyAPI) QueryOneTimeKeys(ctx context.Context, req *keyapi.QueryOneTimeKeysRequest, res *keyapi.QueryOneTimeKeysResponse) {

}

func TestSyncAPIAccessTokens(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		testSyncAccessTokens(t, dbType)
	})
}

func testSyncAccessTokens(t *testing.T, dbType test.DBType) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	alice := userapi.Device{
		ID:          "ALICEID",
		UserID:      user.ID,
		AccessToken: "ALICE_BEARER_TOKEN",
		DisplayName: "Alice",
		AccountType: userapi.AccountTypeUser,
	}

	base, close := testrig.CreateBaseDendrite(t, dbType)
	defer close()

	jsctx, _ := base.NATS.Prepare(base.ProcessContext, &base.Cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &base.Cfg.Global.JetStream)
	msgs := toNATSMsgs(t, base, room.Events()...)
	AddPublicRoutes(base, &syncUserAPI{accounts: []userapi.Device{alice}}, &syncRoomserverAPI{rooms: []*test.Room{room}}, &syncKeyAPI{})
	testrig.MustPublishMsgs(t, jsctx, msgs...)

	testCases := []struct {
		name            string
		req             *http.Request
		wantCode        int
		wantJoinedRooms []string
	}{
		{
			name: "missing access token",
			req: test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
				"timeout": "0",
			})),
			wantCode: 401,
		},
		{
			name: "unknown access token",
			req: test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
				"access_token": "foo",
				"timeout":      "0",
			})),
			wantCode: 401,
		},
		{
			name: "valid access token",
			req: test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
				"access_token": alice.AccessToken,
				"timeout":      "0",
			})),
			wantCode:        200,
			wantJoinedRooms: []string{room.ID},
		},
	}
	// TODO: find a better way
	time.Sleep(500 * time.Millisecond)

	for _, tc := range testCases {
		w := httptest.NewRecorder()
		base.PublicClientAPIMux.ServeHTTP(w, tc.req)
		if w.Code != tc.wantCode {
			t.Fatalf("%s: got HTTP %d want %d", tc.name, w.Code, tc.wantCode)
		}
		if tc.wantJoinedRooms != nil {
			var res types.Response
			if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
				t.Fatalf("%s: failed to decode response body: %s", tc.name, err)
			}
			if len(res.Rooms.Join) != len(tc.wantJoinedRooms) {
				t.Errorf("%s: got %v joined rooms, want %v.\nResponse: %+v", tc.name, len(res.Rooms.Join), len(tc.wantJoinedRooms), res)
			}
			t.Logf("res: %+v", res.Rooms.Join[room.ID])

			gotEventIDs := make([]string, len(res.Rooms.Join[room.ID].Timeline.Events))
			for i, ev := range res.Rooms.Join[room.ID].Timeline.Events {
				gotEventIDs[i] = ev.EventID
			}
			test.AssertEventIDsEqual(t, gotEventIDs, room.Events())
		}
	}
}

// Tests what happens when we create a room and then /sync before all events from /createRoom have
// been sent to the syncapi
func TestSyncAPICreateRoomSyncEarly(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		testSyncAPICreateRoomSyncEarly(t, dbType)
	})
}

func testSyncAPICreateRoomSyncEarly(t *testing.T, dbType test.DBType) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	alice := userapi.Device{
		ID:          "ALICEID",
		UserID:      user.ID,
		AccessToken: "ALICE_BEARER_TOKEN",
		DisplayName: "Alice",
		AccountType: userapi.AccountTypeUser,
	}

	base, close := testrig.CreateBaseDendrite(t, dbType)
	defer close()

	jsctx, _ := base.NATS.Prepare(base.ProcessContext, &base.Cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &base.Cfg.Global.JetStream)
	// order is:
	// m.room.create
	// m.room.member
	// m.room.power_levels
	// m.room.join_rules
	// m.room.history_visibility
	msgs := toNATSMsgs(t, base, room.Events()...)
	sinceTokens := make([]string, len(msgs))
	AddPublicRoutes(base, &syncUserAPI{accounts: []userapi.Device{alice}}, &syncRoomserverAPI{rooms: []*test.Room{room}}, &syncKeyAPI{})
	for i, msg := range msgs {
		testrig.MustPublishMsgs(t, jsctx, msg)
		time.Sleep(100 * time.Millisecond)
		w := httptest.NewRecorder()
		base.PublicClientAPIMux.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
			"access_token": alice.AccessToken,
			"timeout":      "0",
		})))
		if w.Code != 200 {
			t.Errorf("got HTTP %d want 200", w.Code)
			continue
		}
		var res types.Response
		if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
			t.Errorf("failed to decode response body: %s", err)
		}
		sinceTokens[i] = res.NextBatch.String()
		if i == 0 { // create event does not produce a room section
			if len(res.Rooms.Join) != 0 {
				t.Fatalf("i=%v got %d joined rooms, want 0", i, len(res.Rooms.Join))
			}
		} else { // we should have that room somewhere
			if len(res.Rooms.Join) != 1 {
				t.Fatalf("i=%v got %d joined rooms, want 1", i, len(res.Rooms.Join))
			}
		}
	}

	// sync with no token "" and with the penultimate token and this should neatly return room events in the timeline block
	sinceTokens = append([]string{""}, sinceTokens[:len(sinceTokens)-1]...)

	t.Logf("waited for events to be consumed; syncing with %v", sinceTokens)
	for i, since := range sinceTokens {
		w := httptest.NewRecorder()
		base.PublicClientAPIMux.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
			"access_token": alice.AccessToken,
			"timeout":      "0",
			"since":        since,
		})))
		if w.Code != 200 {
			t.Errorf("since=%s got HTTP %d want 200", since, w.Code)
		}
		var res types.Response
		if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
			t.Errorf("failed to decode response body: %s", err)
		}
		if len(res.Rooms.Join) != 1 {
			t.Fatalf("since=%s got %d joined rooms, want 1", since, len(res.Rooms.Join))
		}
		t.Logf("since=%s res state:%+v res timeline:%+v", since, res.Rooms.Join[room.ID].State.Events, res.Rooms.Join[room.ID].Timeline.Events)
		gotEventIDs := make([]string, len(res.Rooms.Join[room.ID].Timeline.Events))
		for j, ev := range res.Rooms.Join[room.ID].Timeline.Events {
			gotEventIDs[j] = ev.EventID
		}
		test.AssertEventIDsEqual(t, gotEventIDs, room.Events()[i:])
	}
}

// Test that if we hit /sync we get back presence: online, regardless of whether messages get delivered
// via NATS. Regression test for a flakey test "User sees their own presence in a sync"
func TestSyncAPIUpdatePresenceImmediately(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		testSyncAPIUpdatePresenceImmediately(t, dbType)
	})
}

func testSyncAPIUpdatePresenceImmediately(t *testing.T, dbType test.DBType) {
	user := test.NewUser(t)
	alice := userapi.Device{
		ID:          "ALICEID",
		UserID:      user.ID,
		AccessToken: "ALICE_BEARER_TOKEN",
		DisplayName: "Alice",
		AccountType: userapi.AccountTypeUser,
	}

	base, close := testrig.CreateBaseDendrite(t, dbType)
	base.Cfg.Global.Presence.EnableOutbound = true
	base.Cfg.Global.Presence.EnableInbound = true
	defer close()

	jsctx, _ := base.NATS.Prepare(base.ProcessContext, &base.Cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &base.Cfg.Global.JetStream)
	AddPublicRoutes(base, &syncUserAPI{accounts: []userapi.Device{alice}}, &syncRoomserverAPI{}, &syncKeyAPI{})
	w := httptest.NewRecorder()
	base.PublicClientAPIMux.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
		"access_token": alice.AccessToken,
		"timeout":      "0",
		"set_presence": "online",
	})))
	if w.Code != 200 {
		t.Fatalf("got HTTP %d want %d", w.Code, 200)
	}
	var res types.Response
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Errorf("failed to decode response body: %s", err)
	}
	if len(res.Presence.Events) != 1 {
		t.Fatalf("expected 1 presence events, got: %+v", res.Presence.Events)
	}
	if res.Presence.Events[0].Sender != alice.UserID {
		t.Errorf("sender: got %v want %v", res.Presence.Events[0].Sender, alice.UserID)
	}
	if res.Presence.Events[0].Type != "m.presence" {
		t.Errorf("type: got %v want %v", res.Presence.Events[0].Type, "m.presence")
	}
	if gjson.ParseBytes(res.Presence.Events[0].Content).Get("presence").Str != "online" {
		t.Errorf("content: not online,  got %v", res.Presence.Events[0].Content)
	}

}

// This is mainly what Sytest is doing in "test_history_visibility"
func TestMessageHistoryVisibility(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		testHistoryVisibility(t, dbType)
	})
}

func testHistoryVisibility(t *testing.T, dbType test.DBType) {
	type result struct {
		seeWithoutJoin bool
		seeBeforeJoin  bool
		seeAfterInvite bool
	}

	// create the users
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	bobDev := userapi.Device{
		ID:          "BOBID",
		UserID:      bob.ID,
		AccessToken: "BOD_BEARER_TOKEN",
		DisplayName: "BOB",
	}
	// check guest and normal user accounts
	for _, accType := range []userapi.AccountType{userapi.AccountTypeGuest, userapi.AccountTypeUser} {
		testCases := []struct {
			historyVisibility string
			wantResult        result
		}{
			{
				historyVisibility: "world_readable",
				wantResult: result{
					seeWithoutJoin: true,
					seeBeforeJoin:  true,
					seeAfterInvite: true,
				},
			},
			{
				historyVisibility: "shared",
				wantResult: result{
					seeWithoutJoin: false,
					seeBeforeJoin:  true,
					seeAfterInvite: true,
				},
			},
			{
				historyVisibility: "invited",
				wantResult: result{
					seeWithoutJoin: false,
					seeBeforeJoin:  false,
					seeAfterInvite: true,
				},
			},
			{
				historyVisibility: "joined",
				wantResult: result{
					seeWithoutJoin: false,
					seeBeforeJoin:  false,
					seeAfterInvite: false,
				},
			},
			{
				historyVisibility: "default",
				wantResult: result{
					seeWithoutJoin: false,
					seeBeforeJoin:  true,
					seeAfterInvite: true,
				},
			},
		}

		bobDev.AccountType = accType
		userType := "guest"
		if accType == userapi.AccountTypeUser {
			userType = "real user"
		}

		base, close := testrig.CreateBaseDendrite(t, dbType)
		defer close()

		jsctx, _ := base.NATS.Prepare(base.ProcessContext, &base.Cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &base.Cfg.Global.JetStream)

		AddPublicRoutes(base, &syncUserAPI{accounts: []userapi.Device{bobDev}}, &syncRoomserverAPI{}, &syncKeyAPI{})

		for _, tc := range testCases {
			testname := fmt.Sprintf("%s - %s", tc.historyVisibility, userType)
			t.Run(testname, func(t *testing.T) {
				// create a room with the given visibility
				room := test.NewRoom(t, alice, test.RoomHistoryVisibility(tc.historyVisibility))

				// send the events/messages to NATS to create the rooms
				beforeJoinEv := room.CreateAndInsert(t, alice, "m.room.message", map[string]interface{}{"body": fmt.Sprintf("Before invite in a %s room", tc.historyVisibility)})
				testrig.MustPublishMsgs(t, jsctx, toNATSMsgs(t, base, room.Events()...)...)
				testrig.MustPublishMsgs(t, jsctx, toNATSMsgs(t, base, beforeJoinEv)...)
				time.Sleep(100 * time.Millisecond)

				// There is only one event, we expect only to be able to see this, if the room is world_readable
				w := httptest.NewRecorder()
				base.PublicClientAPIMux.ServeHTTP(w, test.NewRequest(t, "GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), test.WithQueryParams(map[string]string{
					"access_token": bobDev.AccessToken,
					"dir":          "b",
				})))
				if w.Code != 200 {
					t.Logf("%s", w.Body.String())
					t.Fatalf("got HTTP %d want %d", w.Code, 200)
				}
				// We only care about the returned events at this point
				var res struct {
					Chunk []gomatrixserverlib.ClientEvent `json:"chunk"`
				}
				if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
					t.Errorf("failed to decode response body: %s", err)
				}

				verifyEventVisible(t, tc.wantResult.seeWithoutJoin, beforeJoinEv, res.Chunk)

				// Create invite, a message, join the room and create another message.
				msgs := toNATSMsgs(t, base, room.CreateAndInsert(t, alice, "m.room.member", map[string]interface{}{"membership": "invite"}, test.WithStateKey(bob.ID)))
				testrig.MustPublishMsgs(t, jsctx, msgs...)
				afterInviteEv := room.CreateAndInsert(t, alice, "m.room.message", map[string]interface{}{"body": fmt.Sprintf("After invite in a %s room", tc.historyVisibility)})
				msgs = toNATSMsgs(t, base,
					afterInviteEv,
					room.CreateAndInsert(t, bob, "m.room.member", map[string]interface{}{"membership": "join"}, test.WithStateKey(bob.ID)),
					room.CreateAndInsert(t, alice, "m.room.message", map[string]interface{}{"body": fmt.Sprintf("After join in a %s room", tc.historyVisibility)}),
				)
				testrig.MustPublishMsgs(t, jsctx, msgs...)
				time.Sleep(time.Millisecond * 100)

				// Verify the messages after/before invite are visible or not
				w = httptest.NewRecorder()
				base.PublicClientAPIMux.ServeHTTP(w, test.NewRequest(t, "GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), test.WithQueryParams(map[string]string{
					"access_token": bobDev.AccessToken,
					"dir":          "b",
				})))
				if w.Code != 200 {
					t.Logf("%s", w.Body.String())
					t.Fatalf("got HTTP %d want %d", w.Code, 200)
				}
				if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
					t.Errorf("failed to decode response body: %s", err)
				}
				// verify results
				verifyEventVisible(t, tc.wantResult.seeBeforeJoin, beforeJoinEv, res.Chunk)
				verifyEventVisible(t, tc.wantResult.seeAfterInvite, afterInviteEv, res.Chunk)
			})
		}
	}
}

func verifyEventVisible(t *testing.T, wantVisible bool, wantVisibleEvent *gomatrixserverlib.HeaderedEvent, chunk []gomatrixserverlib.ClientEvent) {
	t.Helper()
	if wantVisible {
		found := false
		for _, ev := range chunk {
			if ev.EventID == wantVisibleEvent.EventID() {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected to see event %s but didn't: %+v", wantVisibleEvent.EventID(), chunk)
		}
	} else {
		for _, ev := range chunk {
			if ev.EventID == wantVisibleEvent.EventID() {
				t.Fatalf("expected not to see event %s: %+v", wantVisibleEvent.EventID(), string(ev.Content))
			}
		}
	}
}

func toNATSMsgs(t *testing.T, base *base.BaseDendrite, input ...*gomatrixserverlib.HeaderedEvent) []*nats.Msg {
	result := make([]*nats.Msg, len(input))
	for i, ev := range input {
		var addsStateIDs []string
		if ev.StateKey() != nil {
			addsStateIDs = append(addsStateIDs, ev.EventID())
		}
		result[i] = testrig.NewOutputEventMsg(t, base, ev.RoomID(), api.OutputEvent{
			Type: rsapi.OutputTypeNewRoomEvent,
			NewRoomEvent: &rsapi.OutputNewRoomEvent{
				Event:             ev,
				AddsStateEventIDs: addsStateIDs,
			},
		})
	}
	return result
}
