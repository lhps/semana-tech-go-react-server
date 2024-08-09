package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/lhps/semana-tech-go-react-server/internal/store/pgstore/pgstore"
)

type store interface {
	GetRoom(context.Context, uuid.UUID) (pgstore.Room, error)
	InsertRoom(context.Context, string) (uuid.UUID, error)
	InsertMessage(context.Context, pgstore.InsertMessageParams) (uuid.UUID, error)
	GetRoomMessages(context.Context, uuid.UUID) ([]pgstore.Message, error)
	GetMessage(context.Context, uuid.UUID) (pgstore.Message, error)
	ReactToMessage(context.Context, uuid.UUID) (int64, error)
	RemoveReactionFromMessage(context.Context, uuid.UUID) (int64, error)
	MarkMessageAsAnswered(context.Context, uuid.UUID) error
}

type apiHandler struct {
	q           store
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(store store) http.Handler {
	a := apiHandler{
		q: store,
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			return true
		}},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessage)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactToMessage)
					r.Delete("/react", a.handleRemoveReactFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	a.r = r

	return a
}

const (
	MessageKindMessageCreated           = "message_created"
	MessageKindMessageAnswered          = "message_answered"
	MessageKindMessageReactionIncreased = "message_reaction_increased"
	MessageKindMessageReactionDecreased = "message_reaction_decreased"
)

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type MessageMessageAnswered struct {
	ID string `json:"id"`
}

type MessageMessageReactionIncreased struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type MessageMessageReactionDecreased struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

func (h apiHandler) notifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscribres, ok := h.subscribers[msg.RoomID]
	if !ok || len(subscribres) == 0 {
		return
	}

	for conn, cancel := range subscribres {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upgrade to ws connection", http.StatusBadRequest)

		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()
	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
	}

	slog.Info("new client connected", "room_id", rawRoomID, "client_ip", r.RemoteAddr)

	h.subscribers[rawRoomID][c] = cancel
	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[rawRoomID], c)
	h.mu.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)

		return
	}

	roomID, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) getRawAndParsedMessageID(w http.ResponseWriter, r *http.Request) (string, uuid.UUID) {
	rawMessageID := chi.URLParam(r, "message_id")

	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)

		return "", uuid.UUID{}
	}

	return rawMessageID, messageID
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {

}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, roomID, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)

		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  roomID,
		Message: body.Message,
	})
	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	type response struct {
		ID string `json:"id"`
	}

	sendJSONResponse(w, response{ID: messageID.String()})

	go h.notifyClients(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: rawRoomID,
		Value: MessageMessageCreated{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	_, _, roomID, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		slog.Error("failed to get room messages", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	if messages == nil {
		messages = []pgstore.Message{}
	}

	sendJSONResponse(w, messages)
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, _, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	_, messageID := h.getRawAndParsedMessageID(w, r)

	message, err := h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to get message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	sendJSONResponse(w, message)
}

func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawMessageID, messageID := h.getRawAndParsedMessageID(w, r)

	reactionsCount, err := h.q.ReactToMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to react to message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	sendJSONResponse(w, response{Count: reactionsCount})

	go h.notifyClients(Message{
		Kind:   MessageKindMessageReactionIncreased,
		RoomID: rawRoomID,
		Value: MessageMessageReactionIncreased{
			ID:    rawMessageID,
			Count: reactionsCount,
		},
	})
}

func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawMessageID, messageID := h.getRawAndParsedMessageID(w, r)

	reactionsCount, err := h.q.RemoveReactionFromMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to remove reaction from message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	sendJSONResponse(w, response{Count: reactionsCount})

	go h.notifyClients(Message{
		Kind:   MessageKindMessageReactionDecreased,
		RoomID: rawRoomID,
		Value: MessageMessageReactionDecreased{
			ID:    rawMessageID,
			Count: reactionsCount,
		},
	})
}

func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawMessageID, messageID := h.getRawAndParsedMessageID(w, r)

	message, err := h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to get message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	if message.Answered {
		http.Error(w, "message is already marked as answered", http.StatusBadRequest)

		return
	}

	err = h.q.MarkMessageAsAnswered(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to mark message as answered", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusOK)

	go h.notifyClients(Message{
		Kind:   MessageKindMessageAnswered,
		RoomID: rawRoomID,
		Value: MessageMessageAnswered{
			ID: rawMessageID,
		},
	})

	// type response struct {
	// 	ID       string `json:"id"`
	// 	Answered bool   `json:"answered"`
	// }

	// data, _ := json.Marshal(response{nil})
	// w.Header().Set("Content-Type", "application/json")
	// _, _ = w.Write(data)
}
