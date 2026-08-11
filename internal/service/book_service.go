package service

import (
	"aislide/internal/model"
	"aislide/internal/store"
	"errors"
)

// we use this layer to add: validations, logging, calls to other services

type BookService struct {
	store store.BookStore
}

func NewBookService(s store.BookStore) *BookService {
	return &BookService{
		store: s,
	}
}

func (s *BookService) GetAllBooks() ([]*model.Book, error) {
	return s.store.GetAll()
}

func (s *BookService) GetBookByID(id int) (*model.Book, error) {
	return s.store.GetById(id)
}

func (s *BookService) CreateBook(book model.Book) (*model.Book, error) {
	// Example business logic
	if book.Title == "" {
		return nil, errors.New("title is required")
	}

	return s.store.Create(&book)
}

func (s *BookService) UpdateBook(id int, book model.Book) (*model.Book, error) {
	// Example business logic
	if book.Title == "" {
		return nil, errors.New("title is required")
	}

	return s.store.Update(id, &book)
}

func (s *BookService) DeleteBook(id int) error {
	return s.store.Delete(id)
}
