package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestGetEnv(t *testing.T) {
	// Test with non-existent key
	result := getEnv("NONEXISTENT_KEY_123", "default_value")
	if result != "default_value" {
		t.Errorf("Expected 'default_value', got %q", result)
	}

	// Test with existing environment variable
	if err := os.Setenv("TEST_KEY", "test_value"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	result = getEnv("TEST_KEY", "default_value")
	if result != "test_value" {
		t.Errorf("Expected 'test_value', got %q", result)
	}
	if err := os.Unsetenv("TEST_KEY"); err != nil {
		t.Errorf("Failed to unset environment variable: %v", err)
	}

	// Test with empty environment variable
	if err := os.Setenv("EMPTY_KEY", ""); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	result = getEnv("EMPTY_KEY", "default_value")
	if result != "default_value" {
		t.Errorf("Expected 'default_value' for empty env var, got %q", result)
	}
	if err := os.Unsetenv("EMPTY_KEY"); err != nil {
		t.Errorf("Failed to unset environment variable: %v", err)
	}
}

func TestIndexHandlerWrongPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/wrong-path", nil)
	rr := httptest.NewRecorder()

	indexHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestCreateCardHandlerWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/card", nil)
	rr := httptest.NewRecorder()

	createCardHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestCreateCardHandlerWrongPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/cards", nil)
	rr := httptest.NewRecorder()

	createCardHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("Expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestCardRouterInvalidPaths(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		expectedStatus int
	}{
		{
			name:           "Too few path segments",
			path:           "/card/1",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Invalid card ID",
			path:           "/card/abc/edit",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Invalid action",
			path:           "/card/1/invalid",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rr := httptest.NewRecorder()

			cardRouter(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

func TestHandlersMethodNotAllowed(t *testing.T) {
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, int)
		method  string
	}{
		{
			name:    "moveCardHandler with GET",
			handler: moveCardHandler,
			method:  http.MethodGet,
		},
		{
			name:    "editCardHandler with POST",
			handler: editCardHandler,
			method:  http.MethodPost,
		},
		{
			name:    "updateCardHandler with GET",
			handler: updateCardHandler,
			method:  http.MethodGet,
		},
		{
			name:    "deleteCardHandler with GET",
			handler: deleteCardHandler,
			method:  http.MethodGet,
		},
		{
			name:    "viewCardHandler with POST",
			handler: viewCardHandler,
			method:  http.MethodPost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/test", nil)
			rr := httptest.NewRecorder()

			tt.handler(rr, req, 1)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("Expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
			}
		})
	}
}

func TestUpdateOrderHandlerWrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/card/order", nil)
	rr := httptest.NewRecorder()

	updateOrderHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

// A status that is not one of the three lanes has to be refused here, the way
// createCardHandler and moveCardHandler refuse it. index.html draws a lane for
// each of the three and nothing else, so a card written into a fourth status
// stays in the database and disappears from the board, and no request the
// interface can make brings it back.
//
// The handler is called with db still nil: the check has to happen before the
// UPDATE, so reaching the database at all would panic here rather than pass.
func TestUpdateOrderHandlerRejectsUnknownStatus(t *testing.T) {
	for _, status := range []string{"archived", "", "TODO", "todo "} {
		body := strings.NewReader(`{"status":"` + status + `","order":[1]}`)
		req := httptest.NewRequest(http.MethodPost, "/card/order", body)
		rr := httptest.NewRecorder()

		updateOrderHandler(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("status %q: expected %d, got %d", status, http.StatusBadRequest, rr.Code)
		}
	}
}

func TestValidStatus(t *testing.T) {
	for _, s := range []string{StatusTodo, StatusInProgress, StatusDone} {
		if !validStatus(s) {
			t.Errorf("Expected %q to be a valid status", s)
		}
	}
	for _, s := range []string{"", "archived", "Todo", "done "} {
		if validStatus(s) {
			t.Errorf("Expected %q to be rejected", s)
		}
	}
}
