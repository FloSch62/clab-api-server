// internal/api/lab_handlers.go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/srl-labs/clab-api-server/internal/clab"
	"github.com/srl-labs/clab-api-server/internal/models"
)

// @Summary Deploy Lab
// @Description Deploys a containerlab topology. Requires EITHER 'topologyContent' OR 'topologySourceUrl' in the request body, but not both. The lab will be owned by the authenticated user.
// @Description The 'topologyContent' field now accepts a JSON object for the topology structure instead of YAML.
// @Description Deployment is DENIED if a lab with the target name already exists, UNLESS 'reconfigure=true' is specified AND the authenticated user owns the existing lab.
// @Description Optional deployment flags are provided as query parameters.
// @Tags Labs
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param deploy_request body models.DeployRequest true "Deployment Source: Provide 'topologyContent' OR 'topologySourceUrl'."
// @Param labNameOverride query string false "Overrides the 'name' field within the topology or inferred from URL." example="my-specific-lab-run"
// @Param reconfigure query boolean false "Allow overwriting an existing lab IF owned by the user (default: false)." example="true"
// @Param maxWorkers query int false "Limit concurrent workers (0 or omit for default)." example="4"
// @Param exportTemplate query string false "Custom Go template file for topology data export ('__full' for full export)." example="__full"
// @Param nodeFilter query string false "Comma-separated list of node names to deploy." example="srl1,router2"
// @Param skipPostDeploy query boolean false "Skip post-deploy actions defined for nodes (default: false)." example="false"
// @Param skipLabdirAcl query boolean false "Skip setting extended ACLs on lab directory (default: false)." example="true"
// @Success 200 {object} object "Raw JSON output from 'clab deploy' (or plain text on error)"
// @Failure 400 {object} models.ErrorResponse "Invalid input (e.g., missing/both content/URL, invalid flags/params, invalid topology name)"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 403 {object} models.ErrorResponse "Forbidden (Attempting to reconfigure a lab owned by another user)"
// @Failure 409 {object} models.ErrorResponse "Conflict (Lab already exists and reconfigure=false or not specified)"
// @Failure 500 {object} models.ErrorResponse "Internal server error (e.g., file system errors, clab execution failed)"
// @Router /api/v1/labs [post]
func DeployLabHandler(c *gin.Context) {
	username := c.GetString("username") // Authenticated user
	ctx := c.Request.Context()

	// --- Bind Request Body ---
	var req models.DeployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("DeployLab failed for user '%s': Invalid request body: %v", username, err)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid request body: " + err.Error()})
		return
	}

	// --- Validate Input: Must have Content XOR URL ---
	hasContent := strings.TrimSpace(req.TopologyContent) != ""
	hasUrl := strings.TrimSpace(req.TopologySourceUrl) != ""

	if !hasContent && !hasUrl {
		log.Warnf("DeployLab failed for user '%s': Request body must include either 'topologyContent' or 'topologySourceUrl'", username)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Request body must include either 'topologyContent' or 'topologySourceUrl'"})
		return
	}
	if hasContent && hasUrl {
		log.Warnf("DeployLab failed for user '%s': Request body cannot include both 'topologyContent' and 'topologySourceUrl'", username)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Request body cannot include both 'topologyContent' and 'topologySourceUrl'"})
		return
	}

	// --- Get & Validate Optional Query Parameters ---
	labNameOverride := c.Query("labNameOverride")
	reconfigure := c.Query("reconfigure") == "true" // Simple bool conversion
	maxWorkersStr := c.DefaultQuery("maxWorkers", "0")
	exportTemplate := c.Query("exportTemplate")
	nodeFilter := c.Query("nodeFilter")
	skipPostDeploy := c.Query("skipPostDeploy") == "true"
	skipLabdirAcl := c.Query("skipLabdirAcl") == "true"

	// Validate query param values
	if labNameOverride != "" && !isValidLabName(labNameOverride) {
		log.Warnf("DeployLab failed for user '%s': Invalid characters in labNameOverride query param '%s'", username, labNameOverride)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in 'labNameOverride' query parameter."})
		return
	}
	if !isValidNodeFilter(nodeFilter) {
		log.Warnf("DeployLab failed for user '%s': Invalid characters in nodeFilter query param '%s'", username, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in 'nodeFilter' query parameter."})
		return
	}
	if !isValidExportTemplate(exportTemplate) {
		log.Warnf("DeployLab failed for user '%s': Invalid exportTemplate query param '%s'", username, exportTemplate)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'exportTemplate' query parameter."})
		return
	}
	maxWorkers, err := strconv.Atoi(maxWorkersStr)
	if err != nil || maxWorkers < 0 {
		log.Warnf("DeployLab failed for user '%s': Invalid maxWorkers query param '%s'", username, maxWorkersStr)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'maxWorkers' query parameter: must be a non-negative integer."})
		return
	}

	// --- Determine Effective Lab Name ---
	var effectiveLabName string
	var originalLabName string // Name from topology content, needed for directory structure
	var topoPathForClab string
	var topoContent string

	if hasUrl {
		log.Infof("DeployLab user '%s': Deploying from URL: %s", username, req.TopologySourceUrl)
		_, urlErr := url.ParseRequestURI(req.TopologySourceUrl)
		isShortcut := !strings.Contains(req.TopologySourceUrl, "/") && !strings.Contains(req.TopologySourceUrl, ":")
		if urlErr != nil && !isShortcut && !strings.HasPrefix(req.TopologySourceUrl, "http") {
			log.Warnf("DeployLab failed for user '%s': Invalid topologySourceUrl format: %s", username, req.TopologySourceUrl)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid topologySourceUrl format"})
			return
		}
		topoPathForClab = req.TopologySourceUrl

		// If URL is used, clab determines the name unless overridden.
		if labNameOverride != "" {
			effectiveLabName = labNameOverride
		} else {
			log.Warnf("DeployLab user '%s': Deploying from URL without labNameOverride. Pre-deployment existence check skipped. Clab will handle potential conflicts.", username)
			effectiveLabName = "<determined_by_clab_from_url>" // Placeholder
		}
	} else { // hasContent
		log.Infof("DeployLab user '%s': Deploying from provided topology content.", username)
		jsonContent := strings.TrimSpace(req.TopologyContent)

		// Parse JSON to extract lab name and convert to YAML
		var topoData map[string]interface{}
		err = json.Unmarshal([]byte(jsonContent), &topoData)
		if err != nil {
			log.Warnf("DeployLab failed for user '%s': Invalid topology JSON: %v", username, err)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid topology JSON: " + err.Error()})
			return
		}

		// Extract name from the JSON
		originalLabNameValue, ok := topoData["name"]
		if !ok {
			log.Warnf("DeployLab failed for user '%s': JSON topology missing 'name' field", username)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "JSON topology must contain a top-level 'name' field."})
			return
		}
		originalLabName, ok = originalLabNameValue.(string)
		if !ok || originalLabName == "" {
			log.Warnf("DeployLab failed for user '%s': JSON topology 'name' field is not a non-empty string", username)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Topology 'name' field must be a non-empty string."})
			return
		}
		if !isValidLabName(originalLabName) {
			log.Warnf("DeployLab failed for user '%s': Invalid characters in topology 'name': %s", username, originalLabName)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in topology 'name'."})
			return
		}

		// Convert JSON to YAML for containerlab
		yamlBytes, err := yaml.Marshal(topoData)
		if err != nil {
			log.Warnf("DeployLab failed for user '%s': Failed to convert JSON topology to YAML: %v", username, err)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Failed to convert JSON topology to YAML: " + err.Error()})
			return
		}
		topoContent = string(yamlBytes)

		// Determine effective lab name
		if labNameOverride != "" {
			effectiveLabName = labNameOverride
		} else {
			effectiveLabName = originalLabName
		}
	}

	// --- Pre-Deployment Check: Lab Existence and Ownership (if name is known) ---
	if effectiveLabName != "<determined_by_clab_from_url>" {
		labInfo, exists, checkErr := getLabInfo(ctx, effectiveLabName)
		if checkErr != nil {
			log.Errorf("DeployLab failed for user '%s': Error checking lab '%s' existence: %v", username, effectiveLabName, checkErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Error checking lab '%s' status: %s", effectiveLabName, checkErr.Error())})
			return
		}

		if exists {
			if !reconfigure {
				// Lab exists, reconfigure not requested -> Conflict
				log.Warnf("DeployLab failed for user '%s': Lab '%s' already exists and reconfigure=false.", username, effectiveLabName)
				c.JSON(http.StatusConflict, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' already exists. Use 'reconfigure=true' query parameter to overwrite.", effectiveLabName)})
				return
			} else {
				// Lab exists, reconfigure requested -> Check ownership
				if !isSuperuser(username) && labInfo.Owner != username {
					// User is not owner (and not superuser) -> Forbidden
					log.Warnf("DeployLab failed for user '%s': Attempted to reconfigure lab '%s' owned by '%s'.", username, effectiveLabName, labInfo.Owner)
					c.JSON(http.StatusForbidden, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' exists but is owned by user '%s'. Reconfigure permission denied.", effectiveLabName, labInfo.Owner)})
					return
				}
				// User is owner (or superuser) -> Allow reconfigure
				log.Infof("DeployLab user '%s': Lab '%s' exists, reconfigure=true and ownership confirmed (owner: '%s'). Proceeding with deployment.", username, effectiveLabName, labInfo.Owner)
			}
		} else {
			// Lab does not exist -> Proceed
			log.Infof("DeployLab user '%s': Lab '%s' does not exist. Proceeding with deployment.", username, effectiveLabName)
		}
	}

	// --- Prepare Base Arguments ---
	args := []string{"deploy", "--owner", username}

	// --- Handle Topology Content Saving (if applicable) ---
	if hasContent {
		// --- Get Lab Directory and User UID/GID ---
		targetDir, uid, gid, err := getLabDirectoryInfo(username, originalLabName)
		if err != nil {
			log.Errorf("DeployLab failed for user '%s': %v", username, err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: err.Error()})
			return
		}

		// --- Create Directory, Set Ownership, Write File ---
		targetFilePath := filepath.Join(targetDir, originalLabName+".clab.yml")
		topoPathForClab = targetFilePath // Update path for clab command

		// If reconfiguring, clab handles cleaning containers, but we might need to ensure dir exists
		err = os.MkdirAll(targetDir, 0750)
		if err != nil {
			log.Errorf("DeployLab failed for user '%s': Failed to create lab directory '%s': %v", username, targetDir, err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to create lab directory: %s.", err.Error())})
			return
		}
		err = os.Chown(targetDir, uid, gid)
		if err != nil {
			// Log warning, but proceed. File write/chown might still work.
			log.Warnf("DeployLab user '%s': Failed to set ownership on lab directory '%s': %v. Continuing...", username, targetDir, err)
		}
		err = os.WriteFile(targetFilePath, []byte(strings.TrimSpace(topoContent)), 0640)
		if err != nil {
			log.Errorf("DeployLab failed for user '%s': Failed to write topology file '%s': %v", username, targetFilePath, err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to write topology file: %s.", err.Error())})
			return
		}
		err = os.Chown(targetFilePath, uid, gid)
		if err != nil {
			log.Errorf("DeployLab failed for user '%s': Failed to set ownership on topology file '%s': %v", username, targetFilePath, err)
			_ = os.Remove(targetFilePath) // Attempt cleanup
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to set ownership on topology file: %s.", err.Error())})
			return
		}
		log.Infof("Saved topology and set ownership for user '%s' lab '%s' to '%s'", username, originalLabName, targetFilePath)
	}

	// --- Add Topology Path/URL to Args ---
	args = append(args, "--topo", topoPathForClab)

	// --- Add Optional Flags from Query Params to Args ---
	if labNameOverride != "" {
		args = append(args, "--name", labNameOverride)
	}
	if reconfigure {
		args = append(args, "--reconfigure")
	}
	if maxWorkers > 0 { // Only add if explicitly set > 0
		args = append(args, "--max-workers", strconv.Itoa(maxWorkers))
	}
	if exportTemplate != "" {
		args = append(args, "--export-template", exportTemplate)
	}
	if nodeFilter != "" {
		args = append(args, "--node-filter", nodeFilter)
	}
	if skipPostDeploy {
		args = append(args, "--skip-post-deploy")
	}
	if skipLabdirAcl {
		args = append(args, "--skip-labdir-acl")
	}

	// --- Execute clab deploy ---
	log.Infof("DeployLab user '%s': Executing clab deploy for lab '%s'...", username, effectiveLabName)
	stdout, stderr, err := clab.RunClabCommand(ctx, username, args...)

	// --- Handle command execution results ---
	if stderr != "" {
		log.Warnf("DeployLab user '%s', lab '%s': clab deploy stderr: %s", username, effectiveLabName, stderr)
	}
	if err != nil {
		log.Errorf("DeployLab failed for user '%s', lab '%s': clab deploy command execution error: %v", username, effectiveLabName, err)
		errMsg := fmt.Sprintf("Failed to deploy lab '%s': %s", effectiveLabName, err.Error())
		// Only append stderr to the response if it looks like a significant error message
		if stderr != "" && (strings.Contains(stderr, "level=error") || strings.Contains(stderr, "failed") || strings.Contains(stderr, "panic")) {
			errMsg += "\nstderr: " + stderr
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: errMsg})
		return
	}

	log.Infof("DeployLab user '%s': clab deploy for lab '%s' executed successfully.", username, effectiveLabName)

	// --- Return result ---
	// Try to parse as JSON first, but handle plain text output gracefully
	var result interface{}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		log.Infof("DeployLab user '%s', lab '%s': Output from clab deploy is not valid JSON. Returning as plain text.", username, effectiveLabName)
		// Check if the non-JSON output indicates an error
		if strings.Contains(stdout, "level=error") || strings.Contains(stdout, "failed") {
			c.JSON(http.StatusInternalServerError, gin.H{"output": stdout, "warning": "Deployment finished but output indicates errors and was not valid JSON"})
		} else {
			c.JSON(http.StatusOK, gin.H{"output": stdout})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// @Summary Deploy Lab from Archive
// @Description Deploys a containerlab topology provided as a .zip or .tar.gz archive. The archive must contain the .clab.yml file and any necessary bind-mount files/directories. The lab will be owned by the authenticated user.
// @Description The lab name is taken from the 'labName' query parameter. The archive is extracted to the user's ~/.clab/<labName>/ directory.
// @Description Deployment is DENIED if a lab with the target name already exists, UNLESS 'reconfigure=true' is specified AND the authenticated user owns the existing lab.
// @Tags Labs
// @Security BearerAuth
// @Accept multipart/form-data
// @Produce json
// @Param labArchive formData file true "Lab archive (.zip or .tar.gz) containing topology file and bind mounts."
// @Param labName query string true "Name for the lab. This determines the extraction directory (~/.clab/<labName>)." example="my-archived-lab"
// @Param reconfigure query boolean false "Allow overwriting an existing lab IF owned by the user (default: false)." example="true"
// @Param maxWorkers query int false "Limit concurrent workers (0 or omit for default)." example="4"
// @Param exportTemplate query string false "Custom Go template file for topology data export ('__full' for full export)." example="__full"
// @Param nodeFilter query string false "Comma-separated list of node names to deploy." example="srl1,router2"
// @Param skipPostDeploy query boolean false "Skip post-deploy actions defined for nodes (default: false)." example="false"
// @Param skipLabdirAcl query boolean false "Skip setting extended ACLs on lab directory (default: false)." example="true"
// @Success 200 {object} object "Raw JSON output from 'clab deploy' (or plain text on error)"
// @Failure 400 {object} models.ErrorResponse "Invalid input (e.g., missing archive, invalid labName, invalid archive format, missing topology file in archive)"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 403 {object} models.ErrorResponse "Forbidden (Attempting to reconfigure a lab owned by another user)"
// @Failure 409 {object} models.ErrorResponse "Conflict (Lab already exists and reconfigure=false or not specified)"
// @Failure 500 {object} models.ErrorResponse "Internal server error (e.g., file system errors, extraction errors, clab execution failed)"
// @Router /api/v1/labs/archive [post]
func DeployLabArchiveHandler(c *gin.Context) {
	username := c.GetString("username")
	ctx := c.Request.Context()

	// --- Get Lab Name (Required Query Parameter) ---
	labName := c.Query("labName")
	if labName == "" {
		log.Warnf("DeployLab (Archive) failed for user '%s': Missing required 'labName' query parameter", username)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Missing required 'labName' query parameter."})
		return
	}
	if !isValidLabName(labName) {
		log.Warnf("DeployLab (Archive) failed for user '%s': Invalid characters in labName query param '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in 'labName' query parameter."})
		return
	}
	log.Debugf("DeployLab (Archive) user '%s': Preparing lab '%s'", username, labName)

	// --- Get & Validate Optional Query Parameters ---
	reconfigure := c.Query("reconfigure") == "true"
	maxWorkersStr := c.DefaultQuery("maxWorkers", "0")
	exportTemplate := c.Query("exportTemplate")
	nodeFilter := c.Query("nodeFilter")
	skipPostDeploy := c.Query("skipPostDeploy") == "true"
	skipLabdirAcl := c.Query("skipLabdirAcl") == "true"

	// Validate query param values
	if !isValidNodeFilter(nodeFilter) {
		log.Warnf("DeployLab (Archive) failed for user '%s', lab '%s': Invalid nodeFilter '%s'", username, labName, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in 'nodeFilter' query parameter."})
		return
	}
	if !isValidExportTemplate(exportTemplate) {
		log.Warnf("DeployLab (Archive) failed for user '%s', lab '%s': Invalid exportTemplate '%s'", username, labName, exportTemplate)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'exportTemplate' query parameter."})
		return
	}
	maxWorkers, err := strconv.Atoi(maxWorkersStr)
	if err != nil || maxWorkers < 0 {
		log.Warnf("DeployLab (Archive) failed for user '%s', lab '%s': Invalid maxWorkers '%s'", username, labName, maxWorkersStr)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'maxWorkers' query parameter: must be a non-negative integer."})
		return
	}

	// --- Pre-Extraction Check: Lab Existence and Ownership ---
	labInfo, exists, checkErr := getLabInfo(ctx, labName)
	if checkErr != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s': Error checking lab '%s' existence: %v", username, labName, checkErr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Error checking lab '%s' status: %s", labName, checkErr.Error())})
		return
	}

	// --- Get Lab Directory and User UID/GID ---
	targetDir, uid, gid, err := getLabDirectoryInfo(username, labName)
	if err != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s': %v", username, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: err.Error()})
		return
	}

	if exists {
		if !reconfigure {
			// Lab exists, reconfigure not requested -> Conflict
			log.Warnf("DeployLab (Archive) failed for user '%s': Lab '%s' already exists and reconfigure=false.", username, labName)
			c.JSON(http.StatusConflict, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' already exists. Use 'reconfigure=true' query parameter to overwrite.", labName)})
			return
		} else {
			// Lab exists, reconfigure requested -> Check ownership
			if !isSuperuser(username) && labInfo.Owner != username {
				// User is not owner (and not superuser) -> Forbidden
				log.Warnf("DeployLab (Archive) failed for user '%s': Attempted to reconfigure lab '%s' owned by '%s'.", username, labName, labInfo.Owner)
				c.JSON(http.StatusForbidden, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' exists but is owned by user '%s'. Reconfigure permission denied.", labName, labInfo.Owner)})
				return
			}
			// User is owner (or superuser) -> Allow reconfigure
			log.Infof("DeployLab (Archive) user '%s': Lab '%s' exists, reconfigure=true and ownership confirmed (owner: '%s'). Removing existing directory before extraction.", username, labName, labInfo.Owner)
			// Remove existing directory *before* extraction
			if err := os.RemoveAll(targetDir); err != nil {
				log.Warnf("DeployLab (Archive) user '%s': Failed to remove existing directory '%s' during reconfigure: %v. Continuing...", username, targetDir, err)
				// Don't necessarily fail here, MkdirAll might still work or handle it.
			}
		}
	} else {
		// Lab does not exist -> Proceed
		log.Infof("DeployLab (Archive) user '%s': Lab '%s' does not exist. Proceeding with extraction.", username, labName)
	}
	// --- End Pre-Extraction Check ---

	// --- Prepare Target Directory (Create if not exists after potential removal) ---
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s': Failed to create lab directory '%s': %v", username, targetDir, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to create lab directory."})
		return
	}
	if err := os.Chown(targetDir, uid, gid); err != nil {
		// Log warning but continue. Extraction might still work if API user has permissions.
		log.Warnf("DeployLab (Archive) user '%s': Failed to set ownership on lab directory '%s': %v. Continuing...", username, targetDir, err)
	} else {
		log.Debugf("DeployLab (Archive) user '%s': Ensured lab directory '%s' exists with correct ownership.", username, targetDir)
	}

	// --- Process Uploaded Archive ---
	fileHeader, err := c.FormFile("labArchive") // Field name in the multipart form
	if err != nil {
		if err == http.ErrMissingFile {
			log.Warnf("DeployLab (Archive) failed for user '%s': Missing 'labArchive' file in request", username)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Missing 'labArchive' file in multipart form data."})
		} else {
			log.Warnf("DeployLab (Archive) failed for user '%s': Error retrieving 'labArchive' file: %v", username, err)
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Error retrieving 'labArchive' file: " + err.Error()})
		}
		_ = os.RemoveAll(targetDir) // Clean up created directory if upload failed
		return
	}

	// --- Open the uploaded file ---
	archiveFile, err := fileHeader.Open()
	if err != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s': Cannot open uploaded archive '%s': %v", username, fileHeader.Filename, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Cannot open uploaded archive."})
		_ = os.RemoveAll(targetDir) // Clean up created directory
		return
	}
	defer archiveFile.Close()

	// --- Detect Type and Extract ---
	filename := fileHeader.Filename
	log.Infof("DeployLab (Archive) user '%s': Received archive '%s', size %d. Extracting to '%s'", username, filename, fileHeader.Size, targetDir)

	var extractionErr error

	if strings.HasSuffix(strings.ToLower(filename), ".zip") {
		extractionErr = extractZip(archiveFile, fileHeader.Size, targetDir, uid, gid)
	} else if strings.HasSuffix(strings.ToLower(filename), ".tar.gz") || strings.HasSuffix(strings.ToLower(filename), ".tgz") {
		extractionErr = extractTarGz(archiveFile, targetDir, uid, gid)
	} else {
		log.Warnf("DeployLab (Archive) failed for user '%s': Unsupported archive format for file '%s'", username, filename)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: fmt.Sprintf("Unsupported archive format: %s. Use .zip or .tar.gz.", filename)})
		_ = os.RemoveAll(targetDir) // Clean up created directory
		return
	}

	// --- Handle Extraction Errors ---
	if extractionErr != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s': Error extracting archive '%s': %v", username, filename, extractionErr)
		// Attempt to clean up partially extracted directory
		_ = os.RemoveAll(targetDir)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to extract archive: %s", extractionErr.Error())})
		return
	}
	log.Infof("DeployLab (Archive) user '%s': Successfully extracted archive '%s' to '%s'", username, filename, targetDir)

	// --- Find Topology File within extracted directory ---
	topoPathForClab := ""
	// First, look for a file named exactly <labName>.clab.yml
	expectedTopoPath := filepath.Join(targetDir, labName+".clab.yml")
	if _, err := os.Stat(expectedTopoPath); err == nil {
		topoPathForClab = expectedTopoPath
		log.Debugf("DeployLab (Archive) user '%s': Found topology file matching lab name: '%s'", username, topoPathForClab)
	} else {
		// If not found, search for the first *.clab.yml or *.clab.yaml file in the root of targetDir
		entries, readErr := os.ReadDir(targetDir)
		if readErr != nil {
			log.Errorf("DeployLab (Archive) failed for user '%s': Cannot read extracted directory '%s': %v", username, targetDir, readErr)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to read extracted lab directory."})
			_ = os.RemoveAll(targetDir)
			return
		}
		for _, entry := range entries {
			entryNameLower := strings.ToLower(entry.Name())
			if !entry.IsDir() && (strings.HasSuffix(entryNameLower, ".clab.yml") || strings.HasSuffix(entryNameLower, ".clab.yaml")) {
				topoPathForClab = filepath.Join(targetDir, entry.Name())
				log.Debugf("DeployLab (Archive) user '%s': Found topology file by suffix: '%s'", username, topoPathForClab)
				break // Use the first one found
			}
		}
	}

	if topoPathForClab == "" {
		log.Errorf("DeployLab (Archive) failed for user '%s': No '*.clab.yml' or '*.clab.yaml' file found in the root of the extracted archive in '%s'.", username, targetDir)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "No '*.clab.yml' or '*.clab.yaml' file found in the root of the archive."})
		_ = os.RemoveAll(targetDir)
		return
	}

	// --- Construct clab deploy args ---
	args := []string{"deploy", "--owner", username, "--topo", topoPathForClab}

	// Add optional flags
	if reconfigure {
		// Note: We already removed the dir if needed, but --reconfigure tells clab to also remove containers first
		args = append(args, "--reconfigure")
	}
	if maxWorkers > 0 {
		args = append(args, "--max-workers", strconv.Itoa(maxWorkers))
	}
	if exportTemplate != "" {
		args = append(args, "--export-template", exportTemplate)
	}
	if nodeFilter != "" {
		args = append(args, "--node-filter", nodeFilter)
	}
	if skipPostDeploy {
		args = append(args, "--skip-post-deploy")
	}
	if skipLabdirAcl {
		args = append(args, "--skip-labdir-acl")
	}

	// --- Execute clab deploy ---
	log.Infof("DeployLab (Archive) user '%s': Executing clab deploy for lab '%s' using topology '%s'...", username, labName, topoPathForClab)
	stdout, stderr, err := clab.RunClabCommand(ctx, username, args...)

	// --- Handle command execution results ---
	if stderr != "" {
		log.Warnf("DeployLab (Archive) user '%s', lab '%s': clab deploy stderr: %s", username, labName, stderr)
	}
	if err != nil {
		log.Errorf("DeployLab (Archive) failed for user '%s', lab '%s': clab deploy command execution error: %v", username, labName, err)
		errMsg := fmt.Sprintf("Failed to deploy lab '%s' from archive: %s", labName, err.Error())
		if stderr != "" && (strings.Contains(stderr, "level=error") || strings.Contains(stderr, "failed") || strings.Contains(stderr, "panic")) {
			errMsg += "\nstderr: " + stderr
		}
		// Don't remove targetDir here, deployment failed but extraction succeeded, user might want to inspect
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: errMsg})
		return
	}

	log.Infof("DeployLab (Archive) user '%s': clab deploy for lab '%s' executed successfully.", username, labName)

	// --- Process and return result ---
	// Try to parse as JSON first, but handle plain text output gracefully
	var result interface{}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		log.Infof("DeployLab (Archive) user '%s', lab '%s': Output from clab deploy is not valid JSON. Returning as plain text.", username, labName)
		// Check if the non-JSON output indicates an error
		if strings.Contains(stdout, "level=error") || strings.Contains(stdout, "failed") {
			c.JSON(http.StatusInternalServerError, gin.H{"output": stdout, "warning": "Deployment finished but output indicates errors and was not valid JSON"})
		} else {
			c.JSON(http.StatusOK, gin.H{"output": stdout})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// @Summary Destroy Lab
// @Description Destroys a lab by name, checking ownership via 'owner' field from clab inspect.
// @Description Optionally cleans up the lab directory (~/.clab/<labname>) if 'cleanup=true' is passed and the API deployed it from content.
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Param labName path string true "Name of the lab to destroy" example="my-test-lab"
// @Param cleanup query boolean false "Remove lab directory (~/.clab/<labname>) after destroy (default: false)" example="true"
// @Param graceful query boolean false "Attempt graceful shutdown of containers (default: false)" example="true"
// @Param keepMgmtNet query boolean false "Keep the management network (default: false)" example="true"
// @Param nodeFilter query string false "Destroy only specific nodes (comma-separated)" example="srl1,srl2"
// @Success 200 {object} models.GenericSuccessResponse
// @Failure 400 {object} models.ErrorResponse "Invalid lab name or node filter"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName} [delete]
func DestroyLabHandler(c *gin.Context) {
	username := c.GetString("username") // Authenticated user
	labName := c.Param("labName")

	// --- Validate Path Param ---
	if !isValidLabName(labName) {
		log.Warnf("DestroyLab failed for user '%s': Invalid characters in lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name. Use alphanumeric, hyphen, underscore."})
		return
	}
	log.Debugf("DestroyLab user '%s': Attempting to destroy lab '%s'", username, labName)

	// --- Get & Validate Query Params ---
	cleanup := c.Query("cleanup") == "true"
	graceful := c.Query("graceful") == "true"
	keepMgmtNet := c.Query("keepMgmtNet") == "true"
	nodeFilter := c.Query("nodeFilter")

	if !isValidNodeFilter(nodeFilter) {
		log.Warnf("DestroyLab failed for user '%s', lab '%s': Invalid nodeFilter '%s'", username, labName, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in nodeFilter. Use comma-separated alphanumeric, hyphen, underscore."})
		return
	}

	// --- Verify lab exists and belongs to the user via inspect + owner field check ---
	originalTopoPath, ownerCheckErr := verifyLabOwnership(c, username, labName) // Extract ownership check
	if ownerCheckErr != nil {
		// verifyLabOwnership already sent the response
		return
	}
	// Ownership confirmed

	// --- Execute clab destroy ---
	destroyArgs := []string{"destroy", "--name", labName}
	if graceful {
		destroyArgs = append(destroyArgs, "--graceful")
	}
	if keepMgmtNet {
		destroyArgs = append(destroyArgs, "--keep-mgmt-net")
	}
	if nodeFilter != "" {
		destroyArgs = append(destroyArgs, "--node-filter", nodeFilter)
	}
	// NOTE: --cleanup is handled *after* the command by removing the directory

	log.Infof("DestroyLab user '%s': Executing clab destroy for lab '%s' (cleanup=%t)...", username, labName, cleanup)
	_, stderr, err := clab.RunClabCommand(c.Request.Context(), username, destroyArgs...)

	// Handle clab destroy command result
	if err != nil {
		log.Errorf("DestroyLab failed for user '%s': clab destroy command failed for lab '%s': %v", username, labName, err)
		errMsg := fmt.Sprintf("Failed to destroy lab '%s': %s", labName, err.Error())
		if stderr != "" {
			errMsg += "\nstderr: " + stderr
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: errMsg})
		return
	}

	// clab destroy succeeded
	log.Infof("Lab '%s' destroyed successfully via clab for user '%s'.", labName, username)
	if stderr != "" { // Log stderr even on success
		log.Warnf("DestroyLab user '%s': clab destroy stderr for lab '%s' (command succeeded): %s", username, labName, stderr)
	}

	// --- Attempt to Cleanup Topology Directory if requested AND if deployed via content ---
	if cleanup {
		// Only cleanup if we know the original path (meaning it was likely deployed via content by the API)
		// We get originalTopoPath from the verifyLabOwnership call which gets it from inspect.
		// We need to derive the *directory* from the path.
		if originalTopoPath != "" && !strings.HasPrefix(originalTopoPath, "http") && !strings.Contains(originalTopoPath, "://") { // Basic check it's a local path
			targetDir := filepath.Dir(originalTopoPath)

			// Sanity check the directory
			// If CLAB_SHARED_LABS_DIR is set, ensure it's within that structure
			// Otherwise, ensure it's within the user's ~/.clab structure
			sharedDir := os.Getenv("CLAB_SHARED_LABS_DIR")
			expectedBase := ""

			if sharedDir != "" {
				// Using shared directory
				expectedBase = filepath.Join(sharedDir, "users", username)
			} else {
				// Using user's home directory
				usr, lookupErr := user.Lookup(username)
				if lookupErr == nil {
					expectedBase = filepath.Join(usr.HomeDir, ".clab")
				}
			}

			if expectedBase != "" && strings.HasPrefix(targetDir, expectedBase) && targetDir != expectedBase {
				log.Infof("DestroyLab user '%s': Cleanup requested. Removing directory: %s", username, targetDir)
				cleanupErr := os.RemoveAll(targetDir)
				if cleanupErr != nil {
					// Log error but don't make the API call fail, main task (destroy) succeeded.
					log.Warnf("Failed to cleanup topology directory '%s' for user '%s' after destroy: %v. API server might lack permissions.", targetDir, username, cleanupErr)
				} else {
					log.Infof("Successfully cleaned up topology directory '%s' for user '%s'", targetDir, username)
				}
			} else {
				log.Warnf("DestroyLab user '%s': Cleanup requested but skipping directory removal for path '%s'. Reason: Path is not within expected structure or original path unknown/remote.", username, targetDir)
			}
		} else {
			log.Infof("DestroyLab user '%s': Cleanup requested but skipping directory removal. Reason: Lab likely deployed from URL or original path unknown.", username)
		}
	} else {
		log.Infof("DestroyLab user '%s': Cleanup not requested. Lab directory (if any) retained.", username)
	}

	c.JSON(http.StatusOK, models.GenericSuccessResponse{Message: fmt.Sprintf("Lab '%s' destroyed successfully", labName)})
}

// @Summary Redeploy Lab
// @Description Redeploys a lab by name, effectively running destroy and then deploy. Checks ownership.
// @Description Uses the original topology file path found during inspection. Optional flags are provided via query parameters.
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Param labName path string true "Name of the lab to redeploy" example:"my-test-lab"
// @Param cleanup query boolean false "Remove containerlab artifacts before deploy (default: false)" example:"false"
// @Param graceful query boolean false "Attempt graceful shutdown of containers (default: false)" example:"true"
// @Param graph query boolean false "Generate graph during redeploy (default: false)" example:"false"
// @Param network query string false "Override management network name" example:"my-custom-mgmt-net"
// @Param ipv4Subnet query string false "Override management network IPv4 subnet (CIDR)" example:"172.30.30.0/24"
// @Param ipv6Subnet query string false "Override management network IPv6 subnet (CIDR)" example:"2001:172:30:30::/64"
// @Param maxWorkers query int false "Limit concurrent workers (0 or omit for default)." example:"4"
// @Param keepMgmtNet query boolean false "Keep the management network during destroy phase (default: false)" example:"true"
// @Param skipPostDeploy query boolean false "Skip post-deploy actions defined for nodes (default: false)" example:"false"
// @Param exportTemplate query string false "Custom Go template file for topology data export ('__full' for full export)." example:"__full"
// @Param skipLabdirAcl query boolean false "Skip setting extended ACLs on lab directory (default: false)" example:"true"
// @Success 200 {object} object "Raw output from 'clab redeploy'"
// @Failure 400 {object} models.ErrorResponse "Invalid lab name or query parameter options"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName} [put]
func RedeployLabHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")

	// --- Validate Path Param ---
	if !isValidLabName(labName) {
		log.Warnf("RedeployLab failed for user '%s': Invalid characters in lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return
	}

	// --- Get & Validate Optional Query Parameters ---
	cleanup := c.Query("cleanup") == "true"
	graceful := c.Query("graceful") == "true"
	graph := c.Query("graph") == "true" // Added from model
	network := c.Query("network")
	ipv4Subnet := c.Query("ipv4Subnet")
	ipv6Subnet := c.Query("ipv6Subnet")
	maxWorkersStr := c.DefaultQuery("maxWorkers", "0")
	keepMgmtNet := c.Query("keepMgmtNet") == "true"
	skipPostDeploy := c.Query("skipPostDeploy") == "true"
	exportTemplate := c.Query("exportTemplate")
	skipLabdirAcl := c.Query("skipLabdirAcl") == "true"

	// Validate query param values
	if !isValidExportTemplate(exportTemplate) {
		log.Warnf("RedeployLab failed for user '%s', lab '%s': Invalid exportTemplate query param '%s'", username, labName, exportTemplate)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'exportTemplate' query parameter."})
		return
	}
	maxWorkers, err := strconv.Atoi(maxWorkersStr)
	if err != nil || maxWorkers < 0 {
		log.Warnf("RedeployLab failed for user '%s', lab '%s': Invalid maxWorkers query param '%s'", username, labName, maxWorkersStr)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid 'maxWorkers' query parameter: must be a non-negative integer."})
		return
	}

	log.Debugf("RedeployLab user '%s': Attempting to redeploy lab '%s' with query params...", username, labName)

	// --- Verify lab exists, belongs to the user, and get original topology path ---
	originalTopoPath, ownerCheckErr := verifyLabOwnership(c, username, labName)
	if ownerCheckErr != nil {
		// verifyLabOwnership already sent the response
		return
	}
	if originalTopoPath == "" {
		log.Errorf("RedeployLab failed for user '%s', lab '%s': Could not determine original topology path from inspect output.", username, labName)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Could not determine original topology path needed for redeploy."})
		return
	}
	log.Debugf("RedeployLab user '%s', lab '%s': Using original topology path '%s'", username, labName, originalTopoPath)

	// --- Execute clab redeploy ---
	args := []string{"redeploy", "--topo", originalTopoPath}

	// Add flags based on query parameters
	if cleanup {
		args = append(args, "--cleanup")
	}
	if graceful {
		args = append(args, "--graceful")
	}
	if graph {
		args = append(args, "--graph")
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if ipv4Subnet != "" {
		args = append(args, "--ipv4-subnet", ipv4Subnet)
	}
	if ipv6Subnet != "" {
		args = append(args, "--ipv6-subnet", ipv6Subnet)
	}
	if maxWorkers > 0 { // Only add if explicitly set > 0
		args = append(args, "--max-workers", strconv.Itoa(maxWorkers))
	}
	if keepMgmtNet {
		args = append(args, "--keep-mgmt-net")
	}
	if skipPostDeploy {
		args = append(args, "--skip-post-deploy")
	}
	if exportTemplate != "" {
		args = append(args, "--export-template", exportTemplate)
	}
	if skipLabdirAcl {
		args = append(args, "--skip-labdir-acl")
	}

	log.Infof("RedeployLab user '%s': Executing clab redeploy for lab '%s'...", username, labName)
	stdout, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	// Handle command execution results
	if stderr != "" {
		log.Warnf("RedeployLab user '%s', lab '%s': clab redeploy stderr: %s", username, labName, stderr)
	}
	if err != nil {
		log.Errorf("RedeployLab failed for user '%s', lab '%s': clab redeploy command execution error: %v", username, labName, err)
		errMsg := fmt.Sprintf("Failed to redeploy lab '%s': %s", labName, err.Error())
		// Only append stderr to the response if it looks like a significant error message
		if stderr != "" && (strings.Contains(stderr, "level=error") || strings.Contains(stderr, "failed") || strings.Contains(stderr, "panic")) {
			errMsg += "\nstderr: " + stderr
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: errMsg})
		return
	}

	log.Infof("RedeployLab user '%s': clab redeploy for lab '%s' executed successfully.", username, labName)

	// Return plain text output for redeploy (no JSON format support)
	c.JSON(http.StatusOK, gin.H{"output": stdout})
}

// @Summary Inspect Lab
// @Description Get details about a specific running lab, checking ownership via 'owner' field. Supports '--details'.
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Param labName path string true "Name of the lab to inspect" example="my-test-lab"
// @Param details query boolean false "Include full container details (like docker inspect)" example="true"
// @Success 200 {object} models.ClabInspectOutput "Standard JSON output from 'clab inspect'"
// @Success 200 {object} object "Raw JSON output if 'details=true' is used (structure matches 'docker inspect')"
// @Failure 400 {object} models.ErrorResponse "Invalid lab name"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName} [get]
func InspectLabHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")
	details := c.Query("details") == "true"

	if !isValidLabName(labName) {
		log.Warnf("InspectLab failed for user '%s': Invalid characters in lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return
	}
	log.Debugf("InspectLab user '%s': Inspecting lab '%s' (details=%t)", username, labName, details)

	// --- Verify lab exists and belongs to the user ---
	// verifyLabOwnership implicitly runs inspect --name <labName> and checks ownership.
	// We don't need the returned path here, just the success/failure.
	_, ownerCheckErr := verifyLabOwnership(c, username, labName)
	if ownerCheckErr != nil {
		// verifyLabOwnership already sent the response (404, 500, etc.)
		return
	}
	// Ownership confirmed

	// --- Execute clab inspect again (this time potentially with --details) ---
	// Although verifyLabOwnership ran inspect, we run it again here to easily get
	// the --details output if requested and to ensure we have the latest state.
	args := []string{"inspect", "--name", labName, "--format", "json"}
	if details {
		args = append(args, "--details")
	}

	stdout, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	// Error handling (similar to verifyLabOwnership, but focused on this specific call)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(stdout, "no containers found") ||
			strings.Contains(errMsg, "no containers found") ||
			strings.Contains(errMsg, "no containerlab labs found") ||
			strings.Contains(stderr, "no containers found") ||
			strings.Contains(stderr, "Could not find containers for lab") {
			// This shouldn't happen if verifyLabOwnership passed, but handle defensively
			log.Warnf("InspectLab user '%s': Lab '%s' not found on second inspect call (after ownership check).", username, labName)
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' not found.", labName)})
			return
		}
		log.Errorf("InspectLab failed for user '%s': Second clab inspect command failed for lab '%s': %v. Stderr: %s", username, labName, err, stderr)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to inspect lab '%s': %s", labName, err.Error())})
		return
	}
	if stderr != "" {
		log.Warnf("InspectLab user '%s': Second clab inspect stderr for lab '%s': %s", username, labName, stderr)
	}

	// --- Parse and Return Result based on --details flag ---
	if details {
		// Parse into the map structure for details
		var resultMap models.ClabInspectOutputDetails
		if err := json.Unmarshal([]byte(stdout), &resultMap); err != nil {
			log.Errorf("InspectLab failed for user '%s': Failed to parse clab inspect --details JSON output for lab '%s': %v", username, labName, err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to parse clab inspect --details output: " + err.Error()})
			return
		}
		// Extract the array for the specific lab
		labDetails, found := resultMap[labName]
		if !found {
			// Should not happen if verifyOwnership passed, but handle defensively
			log.Errorf("InspectLab user '%s': Lab '%s' key missing in --details output after ownership check.", username, labName)
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' details not found after ownership check.", labName)})
			return
		}
		log.Debugf("InspectLab user '%s': Inspection (with details) of lab '%s' successful.", username, labName)
		c.JSON(http.StatusOK, labDetails) // Return the array of raw messages

	} else {
		// Parse into the standard map structure
		var resultMap models.ClabInspectOutput
		if err := json.Unmarshal([]byte(stdout), &resultMap); err != nil {
			log.Errorf("InspectLab failed for user '%s': Failed to parse clab inspect JSON output for lab '%s': %v", username, labName, err)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to parse clab inspect output: " + err.Error()})
			return
		}
		// Extract the array for the specific lab
		labContainers, found := resultMap[labName]
		if !found {
			// Should not happen if verifyOwnership passed, but handle defensively
			log.Errorf("InspectLab user '%s': Lab '%s' key missing in standard inspect output after ownership check.", username, labName)
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' details not found after ownership check.", labName)})
			return
		}
		log.Debugf("InspectLab user '%s': Inspection of lab '%s' successful.", username, labName)
		c.JSON(http.StatusOK, labContainers) // Return the array of ClabContainerInfo
	}
}

// @Summary List Lab Interfaces
// @Description Get network interface details for nodes in a specific lab, checking ownership.
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Param labName path string true "Name of the lab" example="my-test-lab"
// @Param node query string false "Filter interfaces for a specific node name" example="clab-my-test-lab-srl1"
// @Success 200 {object} models.ClabInspectInterfacesOutput "JSON output from 'clab inspect interfaces'"
// @Failure 400 {object} models.ErrorResponse "Invalid lab name or node name"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName}/interfaces [get]
func InspectInterfacesHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")
	nodeFilter := c.Query("node") // Optional node name filter

	// --- Validate Path Param ---
	if !isValidLabName(labName) {
		log.Warnf("InspectInterfaces failed for user '%s': Invalid characters in lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return
	}
	// Basic validation for node filter if provided (allow containerlab default names)
	if nodeFilter != "" && !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(nodeFilter) {
		log.Warnf("InspectInterfaces failed for user '%s', lab '%s': Invalid characters in node query param '%s'", username, labName, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in node query parameter."})
		return
	}

	log.Debugf("InspectInterfaces user '%s': Inspecting interfaces for lab '%s' (node filter: '%s')", username, labName, nodeFilter)

	// --- Verify lab exists and belongs to the user ---
	_, ownerCheckErr := verifyLabOwnership(c, username, labName)
	if ownerCheckErr != nil {
		return // verifyLabOwnership sent response
	}
	// Ownership confirmed

	// --- Execute clab inspect interfaces ---
	args := []string{"inspect", "interfaces", "--name", labName, "--format", "json"}
	if nodeFilter != "" {
		args = append(args, "--node", nodeFilter)
	}

	stdout, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	if stderr != "" {
		log.Warnf("InspectInterfaces user '%s': clab inspect interfaces stderr for lab '%s': %s", username, labName, stderr)
	}
	if err != nil {
		// Check for "not found" errors specifically for interfaces command if possible
		errMsg := err.Error()
		if strings.Contains(stdout, "no containers found") ||
			strings.Contains(errMsg, "no containers found") ||
			strings.Contains(errMsg, "no containerlab labs found") ||
			strings.Contains(stderr, "no containers found") ||
			strings.Contains(stderr, "Could not find containers for lab") {
			log.Infof("InspectInterfaces user '%s': Lab '%s' not found.", username, labName)
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("Lab '%s' not found.", labName)})
			return
		}
		// Check if specific node wasn't found
		if nodeFilter != "" && (strings.Contains(stderr, "container not found") || strings.Contains(errMsg, "container not found")) {
			log.Infof("InspectInterfaces user '%s': Node '%s' not found in lab '%s'.", username, nodeFilter, labName)
			c.JSON(http.StatusNotFound, models.ErrorResponse{Error: fmt.Sprintf("Node '%s' not found in lab '%s'.", nodeFilter, labName)})
			return
		}

		log.Errorf("InspectInterfaces failed for user '%s': clab inspect interfaces command failed for lab '%s': %v", username, labName, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to inspect interfaces for lab '%s': %s", labName, err.Error())})
		return
	}

	// --- Parse and Return Result ---
	var result models.ClabInspectInterfacesOutput
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		log.Errorf("InspectInterfaces failed for user '%s': Failed to parse clab inspect interfaces JSON output for lab '%s': %v", username, labName, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to parse clab inspect interfaces output: " + err.Error()})
		return
	}

	log.Debugf("InspectInterfaces user '%s': Inspection of interfaces for lab '%s' successful.", username, labName)
	c.JSON(http.StatusOK, result)
}

// @Summary List All Labs
// @Description Get details about all running labs, filtered by the 'owner' field matching the authenticated user (unless user is in SUPERUSER_GROUP).
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Success 200 {object} models.ClabInspectOutput "Filtered JSON output from 'clab inspect --all'"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs [get]
func ListLabsHandler(c *gin.Context) {
	username := c.GetString("username")  // Authenticated user
	isSuperuser := isSuperuser(username) // Use helper

	if isSuperuser {
		log.Infof("ListLabs user '%s': Identified as superuser. Bypassing owner filtering.", username)
	} else {
		log.Debugf("ListLabs user '%s': Not a superuser. Applying owner filtering.", username)
	}

	log.Debugf("ListLabs user '%s': Listing labs via 'clab inspect --all'...", username)
	args := []string{"inspect", "--all", "--format", "json"}
	stdout, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	if stderr != "" {
		log.Warnf("ListLabs user '%s': clab inspect --all stderr: %s", username, stderr)
	}
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(stdout, "no containers found") ||
			strings.Contains(errMsg, "no containerlab labs found") ||
			strings.Contains(stderr, "no containers found") {
			log.Infof("ListLabs user '%s': No labs found via clab inspect.", username)
			// Return empty map for the new structure
			c.JSON(http.StatusOK, models.ClabInspectOutput{})
			return
		}
		log.Errorf("ListLabs failed for user '%s': clab inspect --all command failed: %v", username, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: fmt.Sprintf("Failed to list labs: %s", err.Error())})
		return
	}

	log.Debugf("ListLabs user '%s': inspect --all command successful, parsing...", username)

	// Unmarshal into the new map structure
	var fullResult models.ClabInspectOutput
	if err := json.Unmarshal([]byte(stdout), &fullResult); err != nil {
		log.Errorf("ListLabs failed for user '%s': Failed to parse clab inspect --all JSON output: %v", username, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Failed to parse clab inspect output: " + err.Error()})
		return
	}

	// --- Filter Results (or don't, if superuser) ---
	var finalResult models.ClabInspectOutput

	if isSuperuser {
		log.Debugf("ListLabs user '%s': Superuser returning all %d labs.", username, len(fullResult))
		finalResult = fullResult // Return the full map
	} else {
		// Create a new map to store filtered results
		finalResult = make(models.ClabInspectOutput)
		labsFoundForUser := make(map[string]bool) // Track labs already processed

		for labName, containers := range fullResult {
			if _, checked := labsFoundForUser[labName]; checked {
				continue // Already decided on this lab
			}

			userOwnsThisLab := false
			for _, cont := range containers {
				if cont.Owner == username {
					userOwnsThisLab = true
					break // Found one owned container, the whole lab is included
				}
			}

			if userOwnsThisLab {
				log.Debugf("ListLabs user '%s': Including lab '%s' as it contains containers owned by the user.", username, labName)
				finalResult[labName] = containers // Add the full container list for this lab
				labsFoundForUser[labName] = true
			} else {
				// Log only once per lab that's being filtered out
				if len(containers) > 0 { // Avoid logging for potentially empty labs
					log.Debugf("ListLabs user '%s': Filtering out lab '%s' as no containers are owned by the user (e.g., owned by '%s').", username, labName, containers[0].Owner)
				}
				labsFoundForUser[labName] = false
			}
		}
		log.Infof("ListLabs user '%s': Found %d labs containing containers owned by the user.", username, len(finalResult))
	}

	c.JSON(http.StatusOK, finalResult) // Return the potentially filtered map
}

// @Summary Save Lab Configuration
// @Description Saves the running configuration for nodes in a specific lab. Checks ownership.
// @Tags Labs
// @Security BearerAuth
// @Produce json
// @Param labName path string true "Name of the lab to save configuration for" example="my-test-lab"
// @Param nodeFilter query string false "Save config only for specific nodes (comma-separated)" example="srl1,srl2"
// @Success 200 {object} models.SaveConfigResponse "Configuration save command executed, includes detailed output."
// @Failure 400 {object} models.ErrorResponse "Invalid lab name or node filter"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName}/save [post]
func SaveLabConfigHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")
	nodeFilter := c.Query("nodeFilter")

	// --- Validate Inputs ---
	if !isValidLabName(labName) {
		log.Warnf("SaveLabConfig failed for user '%s': Invalid lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return
	}
	if !isValidNodeFilter(nodeFilter) {
		log.Warnf("SaveLabConfig failed for user '%s', lab '%s': Invalid nodeFilter '%s'", username, labName, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in nodeFilter."})
		return
	}
	log.Debugf("SaveLabConfig user '%s': Attempting to save config for lab '%s' (filter: '%s')", username, labName, nodeFilter)

	// --- Verify Ownership ---
	originalTopoPath, ownerCheckErr := verifyLabOwnership(c, username, labName)
	if ownerCheckErr != nil {
		return
	}
	if originalTopoPath == "" {
		log.Errorf("SaveLabConfig failed for user '%s', lab '%s': Could not determine original topology path from inspect output.", username, labName)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Could not determine original topology path needed for save."})
		return
	}
	// Ownership confirmed

	// --- Execute clab save ---
	args := []string{"save", "-t", originalTopoPath}
	if nodeFilter != "" {
		args = append(args, "--node-filter", nodeFilter)
	}

	log.Infof("SaveLabConfig user '%s': Executing clab save for lab '%s' using topology '%s'...", username, labName, originalTopoPath)
	_, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	// Handle command execution results
	// Log stderr regardless of error, as it contains the output
	if stderr != "" {
		log.Infof("SaveLabConfig user '%s', lab '%s': clab save output (stderr): %s", username, labName, stderr) // Log as Info now
	}
	if err != nil {
		log.Errorf("SaveLabConfig failed for user '%s', lab '%s': clab save command execution error: %v", username, labName, err)
		errMsg := fmt.Sprintf("Failed to save config for lab '%s': %s", labName, err.Error())
		// Append stderr *if* it seems like an actual error beyond normal output
		if stderr != "" && (strings.Contains(stderr, "level=error") || strings.Contains(stderr, "failed") || strings.Contains(stderr, "panic")) {
			errMsg += "\nstderr: " + stderr
		} else if stderr != "" { // Include normal stderr in error response if command failed
			errMsg += "\nOutput:\n" + stderr
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: errMsg})
		return
	}

	log.Infof("SaveLabConfig user '%s': clab save for lab '%s' executed successfully.", username, labName)

	c.JSON(http.StatusOK, models.SaveConfigResponse{
		Message: fmt.Sprintf("Configuration save command executed successfully for lab '%s'.", labName),
		Output:  stderr, // Include the captured stderr content
	})
}

// @Summary Execute Command in Lab
// @Description Executes a command on nodes within a specific lab. Checks ownership. Supports filtering by a single node name.
// @Tags Labs
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param labName path string true "Name of the lab where the command should be executed" example="my-test-lab"
// @Param nodeFilter query string false "Execute only on this specific node (must match container name, e.g., clab-my-test-lab-srl1)" example="clab-my-test-lab-srl1"
// @Param format query string false "Output format ('plain' or 'json'). Default is 'json'." example="json"
// @Param exec_request body models.ExecRequest true "Command to execute"
// @Success 200 {object} models.ExecResponse "Structured output (if format=json)"
// @Success 200 {string} string "Plain text output (if format=plain)"
// @Failure 400 {object} models.ErrorResponse "Invalid input (lab name, node filter, format, request body)"
// @Failure 401 {object} models.ErrorResponse "Unauthorized"
// @Failure 404 {object} models.ErrorResponse "Lab not found or not owned by user"
// @Failure 500 {object} models.ErrorResponse "Internal server error or clab execution failed"
// @Router /api/v1/labs/{labName}/exec [post]
func ExecCommandHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := c.Param("labName")
	nodeFilter := c.Query("nodeFilter") // Expecting a single container name here
	outputFormat := c.DefaultQuery("format", "json")

	// --- Validate Inputs ---
	if !isValidLabName(labName) {
		log.Warnf("ExecCommand failed for user '%s': Invalid lab name '%s'", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in lab name."})
		return
	}
	if nodeFilter != "" && !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(nodeFilter) {
		log.Warnf("ExecCommand failed for user '%s', lab '%s': Invalid characters in nodeFilter query param '%s'", username, labName, nodeFilter)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid characters in nodeFilter query parameter (expecting single container name)."})
		return
	}
	if outputFormat != "plain" && outputFormat != "json" {
		log.Warnf("ExecCommand failed for user '%s', lab '%s': Invalid format query param '%s'", username, labName, outputFormat)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid format query parameter. Use 'plain' or 'json'."})
		return
	}

	var req models.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("ExecCommand failed for user '%s', lab '%s': Invalid request body: %v", username, labName, err)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		log.Warnf("ExecCommand failed for user '%s', lab '%s': Command cannot be empty", username, labName)
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Command cannot be empty."})
		return
	}

	log.Debugf("ExecCommand user '%s': Attempting to execute on lab '%s' (node filter: '%s', format: '%s')", username, labName, nodeFilter, outputFormat)

	// --- Verify Ownership ---
	originalTopoPath, ownerCheckErr := verifyLabOwnership(c, username, labName)
	if ownerCheckErr != nil {
		return // verifyLabOwnership sent response
	}

	// --- Execute clab exec ---
	args := []string{"exec"}
	if nodeFilter != "" {
		// Use --label filter for single node targeting via container name
		args = append(args, "--label", fmt.Sprintf("clab-node-longname=%s", nodeFilter))
	} else {
		// Target all nodes in the lab using the topology file
		if originalTopoPath == "" {
			log.Errorf("ExecCommand failed for user '%s', lab '%s': Cannot execute on all nodes as original topology path is unknown.", username, labName)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Cannot determine topology path to target all nodes for exec."})
			return
		}
		args = append(args, "--topo", originalTopoPath)
	}

	args = append(args, "--cmd", req.Command)
	if outputFormat == "json" {
		args = append(args, "--format", "json")
	} // 'plain' is the default for clab exec

	log.Infof("ExecCommand user '%s': Executing clab exec for lab '%s'...", username, labName)
	stdout, stderr, err := clab.RunClabCommand(c.Request.Context(), username, args...)

	// --- Handle command execution results ---
	if err != nil {
		log.Warnf("ExecCommand user '%s', lab '%s': clab exec command returned error: %v. Stderr: %s, Stdout: %s", username, labName, err, stderr, stdout)
		// Don't return 500 immediately for plain format if it might be the command failing inside the container
	} else if stderr != "" && outputFormat == "plain" { // Log stderr if plain format, even on success, as it contains the output.
		log.Infof("ExecCommand user '%s', lab '%s': clab exec stderr (contains plain output): %s", username, labName, stderr)
	} else if stderr != "" && outputFormat == "json" { // Log stderr for JSON format only if it's unexpected (exit code 0 usually means no stderr)
		log.Warnf("ExecCommand user '%s', lab '%s': clab exec stderr (json format, exit code 0): %s", username, labName, stderr)
	}

	// --- Process output based on format ---
	if outputFormat == "json" {
		// Declare result using the ExecResponse type
		var result models.ExecResponse
		if jsonErr := json.Unmarshal([]byte(stdout), &result); jsonErr != nil {
			// Parsing failed
			log.Errorf("ExecCommand user '%s', lab '%s': Failed to parse clab exec JSON output: %v. Stdout: %s, Stderr: %s", username, labName, jsonErr, stdout, stderr)
			// Return 500 because the API failed to process valid clab output (or clab output was invalid)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  "Failed to parse clab exec JSON output.",
				"stdout": stdout, // Include raw output for debugging
				"stderr": stderr,
			})
			return
		}
		// Parsing succeeded
		log.Infof("ExecCommand user '%s': clab exec for lab '%s' (json format) successful.", username, labName)
		// Return 200 even if the command *inside* the container failed (result will show non-zero return code)
		c.JSON(http.StatusOK, result)

	} else { // plain format
		// For plain format, clab aggregates stdout/stderr from containers into its *stderr*.
		// If clab itself reported an error (err != nil), something went wrong with clab execution.
		if err != nil {
			// Return 500 as clab itself failed. Include stderr (which might contain clab errors) and stdout.
			responseText := fmt.Sprintf("Clab Error: %s\nStderr:\n%s\nStdout:\n%s", err.Error(), stderr, stdout)
			c.String(http.StatusInternalServerError, responseText)
		} else {
			// Success (exit code 0 from clab). Return clab's stderr as it contains the aggregated output.
			log.Infof("ExecCommand user '%s': clab exec for lab '%s' (plain format) successful, returning stderr content.", username, labName)
			c.String(http.StatusOK, stderr) // Return stderr content for plain format success
		}
	}
}
