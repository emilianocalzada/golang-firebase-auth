package transport

import (
	"aislide/internal/model"
	"aislide/internal/service"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type BookHandler struct {
	service *service.BookService
}

func NewBookHandler(s *service.BookService) *BookHandler {
	return &BookHandler{
		service: s,
	}
}

// RegisterRoutes mounts the book endpoints on the given router.
func (h *BookHandler) RegisterRoutes(r gin.IRouter) {
	books := r.Group("/books")
	books.GET("", h.ListBooks)
	books.POST("", h.CreateBook)
	books.GET("/:id", h.GetBook)
	books.PUT("/:id", h.UpdateBook)
	books.DELETE("/:id", h.DeleteBook)
}

func (h *BookHandler) ListBooks(c *gin.Context) {
	books, err := h.service.GetAllBooks()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, books)
}

func (h *BookHandler) CreateBook(c *gin.Context) {
	var book model.Book
	if err := c.ShouldBindJSON(&book); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}

	created, err := h.service.CreateBook(book)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, created)
}

func (h *BookHandler) GetBook(c *gin.Context) {
	id, ok := bookID(c)
	if !ok {
		return
	}

	book, err := h.service.GetBookByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "book not found"})
		return
	}

	c.JSON(http.StatusOK, book)
}

func (h *BookHandler) UpdateBook(c *gin.Context) {
	id, ok := bookID(c)
	if !ok {
		return
	}

	var book model.Book
	if err := c.ShouldBindJSON(&book); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}

	updated, err := h.service.UpdateBook(id, book)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, updated)
}

func (h *BookHandler) DeleteBook(c *gin.Context) {
	id, ok := bookID(c)
	if !ok {
		return
	}

	if err := h.service.DeleteBook(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

// bookID reads the :id path param and writes a 400 response when it is not a number.
func bookID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid book id"})
		return 0, false
	}

	return id, true
}
