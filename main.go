package main

import (
	"aislide/internal/auth"
	"aislide/internal/config"
	"aislide/internal/revenuecat"
	"aislide/internal/service"
	"aislide/internal/store"
	"aislide/internal/transport"
	"context"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// Database
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := store.Migrate(db); err != nil {
		log.Fatal(err)
	}

	// Firebase ID token verification
	verifier, err := auth.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID, cfg.FirebaseCredentialsFile)
	if err != nil {
		log.Fatal(err)
	}

	// Wire up the layers
	revenueCatClient := revenuecat.New(cfg.RevenueCatSecretAPIKey, cfg.RevenueCatProjectID, cfg.RevenueCatEntitlementID)

	userService := service.NewUserService(store.NewUserStore(db))
	bookService := service.NewBookService(store.NewBookStore(db))
	revenueCatService := service.NewRevenueCatService(userService, store.NewRevenueCatEventStore(db), revenueCatClient)

	authMiddleware := transport.NewAuthMiddleware(verifier, userService)
	userHandler := transport.NewUserHandler(userService)
	bookHandler := transport.NewBookHandler(bookService)
	revenueCatHandler := transport.NewRevenueCatHandler(revenueCatService, cfg.RevenueCatWebhookAuth)

	// Configure the routes
	r := gin.Default()

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Server to server: authenticated by the RevenueCat shared secret, so it
	// stays outside the Firebase-authenticated group.
	revenueCatHandler.RegisterRoutes(r)

	// Everything under /v1 needs a valid Firebase ID token.
	v1 := r.Group("/v1", authMiddleware.RequireAuth())
	userHandler.RegisterRoutes(v1)
	bookHandler.RegisterRoutes(v1)

	// Start listening
	log.Printf("aislide api listening on :%s (db: %s, entitlement: %s)", cfg.Port, cfg.DatabasePath, cfg.RevenueCatEntitlementID)
	log.Fatal(r.Run(":" + cfg.Port))
}
