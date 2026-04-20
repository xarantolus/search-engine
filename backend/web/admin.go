package web

import (
	"log"
	"slices"

	"github.com/gofiber/fiber/v2"
)

// API routes for the Admin Interface
func (s *Server) AdminListPermissions(c *fiber.Ctx) error {
	user, err := s.UserInfo(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
	}

	if !user.IsAdmin {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "You are not an admin"})
	}

	userKeys, err := s.Auth.ListUserKeys()
	if err != nil {
		log.Println("Error getting user keys:", err)
	}

	var userInfos []UserInfo
	for _, key := range userKeys {
		info, err := s.UserInfo(c, key)
		if err != nil {
			log.Println("Error getting user info:", err)
			continue
		}
		userInfos = append(userInfos, info)
	}

	return c.JSON(fiber.Map{"users": userInfos, "permissions": s.Config.PermissionGroups})
}

func (s *Server) AdminSetPermissions(c *fiber.Ctx) error {
	user, err := s.UserInfo(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
	}

	if !user.IsAdmin {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "You are not an admin"})
	}

	var data struct {
		UserID           string   `json:"user_id"`
		PermissionGroups []string `json:"permission_groups"`
	}
	if err := c.BodyParser(&data); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if data.UserID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "user_id is required"})
	}

	userKeys, err := s.Auth.ListUserKeys()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !slices.Contains(userKeys, data.UserID) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "User not found"})
	}

	validGroups := make(map[string]struct{}, len(s.Config.PermissionGroups))
	for _, g := range s.Config.PermissionGroups {
		validGroups[g.Name] = struct{}{}
	}
	for _, g := range data.PermissionGroups {
		if _, ok := validGroups[g]; !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown permission group: " + g})
		}
	}

	userInfo, err := s.UserInfo(c, data.UserID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	userInfo.PermissionGroups = data.PermissionGroups

	if err := s.Auth.SaveUser(c, userInfo); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"success": true})
}

func (s *Server) DeleteDocument(c *fiber.Ctx) error {
	user, err := s.UserInfo(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
	}

	if !user.IsAdmin {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "You are not an admin"})
	}

	docID := c.Params("id")
	if docID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Document ID is required"})
	}

	_, err = s.Index.DeleteDocument(docID, nil)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to delete document"})
	}

	return c.JSON(fiber.Map{"success": true})
}
