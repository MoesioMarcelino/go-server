package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/MoesioMarcelino/go-server/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

type apiHandler struct {
	queries          *pgstore.Queries
	multiRouter      *chi.Mux
	upgrader         websocket.Upgrader
	subscribers      map[string]map[*websocket.Conn]context.CancelFunc
	multualExclusion *sync.Mutex
}

func (handle apiHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handle.multiRouter.ServeHTTP(writer, request)
}

func NewHandler(queries *pgstore.Queries) http.Handler {
	api := apiHandler{
		queries:          queries,
		upgrader:         websocket.Upgrader{CheckOrigin: func(request *http.Request) bool { return true }},
		subscribers:      make(map[string]map[*websocket.Conn]context.CancelFunc),
		multualExclusion: &sync.Mutex{},
	}

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	router.Get("/subscribe/{room_id}", api.handleSubscribe)

	router.Route("/api", func(route chi.Router) {
		route.Route("/rooms", func(route chi.Router) {
			route.Post("/", api.handleCreateRoom)
			route.Get("/", api.handleGetRooms)

			route.Route("/{room_id}/messages", func(route chi.Router) {
				route.Get("/", api.handleGetRoomMessages)
				route.Post("/", api.handleCreateRoomMessage)

				route.Route("/{message_id}", func(route chi.Router) {
					route.Get("/", api.handleGetRoomMessage)
					route.Patch("/react", api.handleReactToMessage)
					route.Delete("/react", api.handleRemoveReactFromMessage)
					route.Delete("/answer", api.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	api.multiRouter = router
	return api
}

const (
	MensageKindMessageCreated = "message_created"
)

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

func (handler apiHandler) notifyClients(msg Message) {
	handler.multualExclusion.Lock()
	defer handler.multualExclusion.Unlock()

	subscribers, ok := handler.subscribers[msg.RoomID]

	if !ok || len(subscribers) == 0 {
		return
	}

	for connection, cancel := range subscribers {
		if err := connection.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}

func (handler apiHandler) handleSubscribe(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	slog.Info(rawRoomId)

	if err != nil {
		slog.Warn("teeste", "error", err)
		http.Error(writer, "Invalid room id", http.StatusBadRequest)
		return
	}

	_, err = handler.queries.GetRoom(request.Context(), roomId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(writer, "Room not found", http.StatusBadRequest)
		}

		http.Error(writer, "Something went wrong", http.StatusInternalServerError)
		return
	}

	connection, err := handler.upgrader.Upgrade(writer, request, nil)

	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(writer, "failed to upgrade to ws connection", http.StatusBadRequest)
	}

	defer connection.Close()

	ctx, cancel := context.WithCancel(request.Context())

	handler.multualExclusion.Lock()

	if _, ok := handler.subscribers[rawRoomId]; !ok {
		handler.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}

	slog.Info("new client connected", "room_id", rawRoomId, "client_ip", request.RemoteAddr)
	handler.subscribers[rawRoomId][connection] = cancel
	handler.multualExclusion.Unlock()

	<-ctx.Done()

	handler.multualExclusion.Lock()
	delete(handler.subscribers[rawRoomId], connection)
	handler.multualExclusion.Unlock()
}

func (handler apiHandler) handleCreateRoom(writer http.ResponseWriter, request *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	roomId, err := handler.queries.InsertRoom(request.Context(), body.Theme)

	if err != nil {
		slog.Warn("failed to insert room", "error", err)
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomId.String()})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)
}

func (handler apiHandler) handleGetRooms(writer http.ResponseWriter, request *http.Request) {
	rooms, err := handler.queries.GetRooms(request.Context())

	if err != nil {
		slog.Error("failed to get rooms", "error", err)
		http.Error(writer, "failed to get rooms", http.StatusInternalServerError)
		return
	}

	type response struct {
		Rooms []pgstore.Room `json:"rooms"`
	}

	data, _ := json.Marshal(response{Rooms: rooms})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)
}

func (handler apiHandler) handleGetRoomMessages(writer http.ResponseWriter, request *http.Request) {}

func (handler apiHandler) handleCreateRoomMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(writer, "Invalid room id", http.StatusBadRequest)
		return
	}

	_, err = handler.queries.GetRoom(request.Context(), roomId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(writer, "Room not found", http.StatusBadRequest)
		}

		http.Error(writer, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	messageID, err := handler.queries.InsertMessage(request.Context(), pgstore.InsertMessageParams{RoomID: roomId, Message: body.Message})

	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(writer, "failed to insert message", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: messageID.String()})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)

	go handler.notifyClients(Message{
		Kind:   MensageKindMessageCreated,
		RoomID: rawRoomId,
		Value: MessageMessageCreated{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})
}
func (handler apiHandler) handleGetRoomMessage(writer http.ResponseWriter, request *http.Request) {}
func (handler apiHandler) handleReactToMessage(writer http.ResponseWriter, request *http.Request) {}
func (handler apiHandler) handleRemoveReactFromMessage(writer http.ResponseWriter, request *http.Request) {
}
func (handler apiHandler) handleMarkMessageAsAnswered(writer http.ResponseWriter, request *http.Request) {
}
