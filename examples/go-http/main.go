// Command go-http serves POST /orders and writes spanlog lines to stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chaugiakhang193/spanlog/go"
)

const (
	address      = "127.0.0.1:3021"
	paymentDelay = 600 * time.Millisecond
)

var errStockNotAvailable = errors.New("stock not available")

type InventoryService struct {
	log   *spanlog.Logger
	mu    sync.Mutex
	stock map[string]int
}

func (s *InventoryService) Reserve(ctx context.Context, sku string, quantity int) (err error) {
	_, done := s.log.Start(ctx, "InventoryService", "Reserve")
	defer done(&err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stock[sku] < quantity {
		return errStockNotAvailable
	}
	s.stock[sku] -= quantity
	return nil
}

type PaymentService struct {
	log *spanlog.Logger
}

func (s *PaymentService) Charge(ctx context.Context) (err error) {
	ctx, done := s.log.Start(ctx, "PaymentService", "Charge")
	defer done(&err)
	select {
	case <-time.After(paymentDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type OrderService struct {
	log       *spanlog.Logger
	inventory *InventoryService
	payment   *PaymentService
}

func (s *OrderService) HandleOrder(ctx context.Context, sku string, quantity int) (err error) {
	ctx, done := s.log.Start(ctx, "OrderService", "HandleOrder")
	defer done(&err)
	if err := s.validate(ctx, sku, quantity); err != nil {
		return err
	}
	if err := s.inventory.Reserve(ctx, sku, quantity); err != nil {
		return err
	}
	return s.payment.Charge(ctx)
}

func (s *OrderService) validate(ctx context.Context, sku string, quantity int) (err error) {
	_, done := s.log.Start(ctx, "OrderService", "validate")
	defer done(&err)
	if sku == "" || quantity <= 0 {
		return errors.New("sku and a positive quantity are required")
	}
	return nil
}

type orderRequest struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

func createOrder(orders *OrderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body orderRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		// spanlog has already reported the error. This branch only answers the client.
		if err := orders.HandleOrder(r.Context(), body.SKU, body.Quantity); err != nil {
			http.Error(w, "order failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

func main() {
	logger := spanlog.New(spanlog.Options{Service: "shop", Level: spanlog.LevelDebug})
	orders := &OrderService{
		log:       logger,
		inventory: &InventoryService{log: logger, stock: map[string]int{"book": 1}},
		payment:   &PaymentService{log: logger},
	}
	mux := http.NewServeMux()
	mux.Handle("POST /orders", createOrder(orders))
	log.Fatal(http.ListenAndServe(address, logger.Middleware(mux)))
}
