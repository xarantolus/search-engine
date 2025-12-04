package web

import (
	"shared/doc"
	"slices"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/meilisearch/meilisearch-go"
)

func (s *Server) Documents(c *fiber.Ctx) error {
	user, err := s.UserInfo(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
	}

	if !user.IsAdmin {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "You are not an admin"})
	}

	var getInt = func(param string, defaultValue int64) int64 {
		value := c.Query(param)
		if value == "" {
			return defaultValue
		}
		intValue, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return defaultValue
		}
		return intValue
	}

	var filter = c.Query("filter")

	var res meilisearch.DocumentsResult
	err = s.Index.GetDocumentsWithContext(c.Context(), &meilisearch.DocumentsQuery{
		Offset: getInt("offset", 0),
		Limit:  getInt("limit", 1000),
		Fields: []string{"id", "url", "inFolderUrl", "content", "slug", "title", "lastModified", "isCode"},
		Filter: filter,
	}, &res)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch documents: " + err.Error()})
	}

	return c.JSON(res)
}

func (s *Server) getDocumentWithAuth(c *fiber.Ctx, fields []string, retrieveVectors bool) (*doc.Document, error) {
	user, err := s.UserInfo(c)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, err.Error())
	}

	id := c.Params("id")
	if id == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "Document ID is required")
	}

	var document doc.Document
	err = s.Index.GetDocumentWithContext(c.Context(), id, &meilisearch.DocumentQuery{
		RetrieveVectors: retrieveVectors,
		Fields:          append([]string{"permissionTag"}, fields...),
	}, &document)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "Failed to fetch document: "+err.Error())
	}
	tags := s.Config.GetGroupsTags(user.PermissionGroups)
	if len(tags) == 0 || !slices.Contains(tags, document.PermissionTag) {
		// Return generic 404 to avoid leaking information
		return nil, fiber.NewError(fiber.StatusNotFound)
	}

	return &document, nil
}

func (s *Server) Document(c *fiber.Ctx) error {
	doc, err := s.getDocumentWithAuth(c, []string{"*"}, true)
	if err != nil {
		return err
	}
	return c.JSON(doc)
}

func (s *Server) DocumentContent(c *fiber.Ctx) error {
	doc, err := s.getDocumentWithAuth(c, []string{"title", "content"}, false)
	if err != nil {
		return err
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	response := "# " + doc.Title + "\n\n" + doc.Content
	return c.SendString(response)
}
