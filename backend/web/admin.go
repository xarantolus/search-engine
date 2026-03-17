package web

import (
	"encoding/json"
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

	// Find all user IDs
	var userids []int64
	bytes, err := s.FiberStore.Get("user_ids")
	if err != nil {
		log.Println("Error getting user_ids:", err)
	}
	err = json.Unmarshal(bytes, &userids)
	if err != nil {
		log.Println("Error unmarshalling user_ids:", err)
	}

	var userInfos []UserInfo
	for _, userid := range userids {
		info, err := s.UserInfo(c, userid)
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
		UserID           int64    `json:"user_id"`
		PermissionGroups []string `json:"permission_groups"`
	}
	err = c.BodyParser(&data)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	// Find all user IDs
	var userids []int64
	bytes, err := s.FiberStore.Get("user_ids")
	if err != nil {
		log.Println("Error getting user_ids:", err)
	}
	err = json.Unmarshal(bytes, &userids)
	if err != nil {
		log.Println("Error unmarshalling user_ids:", err)
	}

	if !slices.Contains(userids, data.UserID) {
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

	err = s.AddLoggedInUser(c, userInfo)
	if err != nil {
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

	_, err = s.Index.DeleteDocument(docID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to delete document"})
	}

	return c.JSON(fiber.Map{"success": true})
}
