/*
Copyright © 2020 Rob Allen <rob@akrabat.com>

Use of this source code is governed by the MIT
license that can be found in the LICENSE file or at
https://akrabat.com/license/mit.
*/

/*
Package cmd implements the commands for the app. In this case, uploading an
image to Flickr.
*/
package commands

import (
	"encoding/json"
	"fmt"
	. "github.com/akrabat/rodeo/internal"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/masci/flickr.v2"
	"gopkg.in/masci/flickr.v2/photos"
	"gopkg.in/masci/flickr.v2/photosets"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const uploadedListBaseFilename = "rodeo-uploaded-files.json"

func init() {
	rootCmd.AddCommand(uploadCmd)

	// Register --force
	uploadCmd.Flags().BoolP("force", "f", false, "Force upload of file even if already uploaded")
	uploadCmd.Flags().BoolP("dry-run", "n", false, "Show what would have been uploaded")
}

// uploadCmd represents the upload command
var uploadCmd = &cobra.Command{
	Use:   "upload <files>...",
	Short: "Upload images to Flickr",
	Long: `Upload images to Flickr

- sets the date uploaded to the creation time of the image so that 
  it appears in the photo stream at the right place.
- sets tags as per exif keywords.
- sets privacy if specific exif-keywords are set.
`,
	Run: func(cmd *cobra.Command, args []string) {

		if len(args) == 0 {
			fmt.Println("Error: At least one file must be specified.")
			os.Exit(2)
		}

		// Read the value of --force (if it is missing, the value is false)
		forceUpload, err := cmd.Flags().GetBool("force")
		if err != nil {
			forceUpload = false
		}

		// Read the value of --force (if it is missing, the value is false)
		dryRun, err := cmd.Flags().GetBool("dry-run")
		if err != nil {
			dryRun = false
		}

		var photoIds []string
		for _, filename := range args {
			photoId := uploadFile(filename, forceUpload, dryRun)
			if photoId != "" {
				photoIds = append(photoIds, photoId)
			}
		}

		fmt.Println("All Done")
		fmt.Printf("View: http://www.flickr.com/photos/%s'\n", viper.GetString("flickr.username"))

		if len(photoIds) > 0 {
			fmt.Printf("Edit: http://www.flickr.com/photos/upload/edit/?ids=%s\n", strings.Join(photoIds, ","))
		}
	},
}

func uploadFile(filename string, forceUpload bool, dryRun bool) string {
	fmt.Println("Processing " + filename)

	config := GetConfig()

	apiKey := config.Flickr.ApiKey
	apiSecret := config.Flickr.ApiSecret
	oauthToken := config.Flickr.OauthToken
	oauthTokenSecret := config.Flickr.OauthSecret
	if apiKey == "" || apiSecret == "" || oauthToken == "" || oauthTokenSecret == "" {
		fmt.Println("Unable to continue. Please run the 'rodeo authenticate' command first")
	}

	exiftool := config.Cmd.Exiftool
	if exiftool == "" {
		fmt.Println("Error: cmd.exiftool needs to be configured.")
		fmt.Println("Config file:", viper.ConfigFileUsed(), "\n")
		os.Exit(2)
	}

	// Has this image been uploaded before?
	if uploadedPhotoId := getUploadedPhotoId(filename, config.Upload.StoreUploadListInImageDir); uploadedPhotoId != "" {
		fmt.Print("This image has already been uploaded to Flickr.")
		if forceUpload == true {
			fmt.Println(" Forcing upload.")
		} else {
			fmt.Printf("\nView this photo: http://www.flickr.com/photos/%s/%s\n", config.Flickr.Username, uploadedPhotoId)
			fmt.Println("")
			return ""
		}
	}

	info, err := GetImageInfo(filename, exiftool)
	if err != nil {
		return ""
	}

	// process rules
	var keywordsToRemove []string
	var keywordsToAdd []string
	var albumsToAddTo []Album

	if config.Rules != nil {
		for _, rule := range config.Rules {
			excludesAll := rule.Condition.ExcludesAll
			excludesAny := rule.Condition.ExcludesAny
			includesAll := rule.Condition.IncludesAll
			includesAny := rule.Condition.IncludesAny

			var intersection []string // applicable keywords from the condition

			// If the list of keywords for this image has all of `excludesAll`, then the rule is ignored
			if len(excludesAll) > 0 {
				intersection = Intersection(info.Keywords, excludesAll)
				if len(intersection) == len(excludesAll) {
					// Every `excludesAll` keyword is in info.Keywords, so this rule does not apply
					//fmt.Println("Excluding due to `excludesAll`")
					continue
				}
				//fmt.Println("`excludesAll` condition does not apply")
			}

			// If the list of keywords for this image has any from `excludesAny`, then the rule is ignored
			if len(excludesAny) > 0 {
				intersection = Intersection(info.Keywords, excludesAny)
				if len(intersection) > 0 {
					// At least one `excludesAny` keyword is in info.Keywords, so this rule does not apply
					//fmt.Println("Excluding due to `excludesAny`")
					continue
				}
				//fmt.Println("`excludesAny` condition does not apply")
			}

			processRules := false
			if len(includesAll) > 0 {
				//  info.Keywords must contain all keywords in `includesAll`
				intersection = Intersection(info.Keywords, includesAll)
				if len(intersection) != len(includesAll) {
					// All `includesAll` keywords do not exist, so this rule does not apply
					//fmt.Println("Excluding due to `includesAll`")
					continue
				}
				//fmt.Println("`includesAll` condition is met")
				processRules = true
			} else if len(includesAny) > 0 {
				//  info.Keywords must contain all keywords in `includesAny`
				intersection = Intersection(info.Keywords, includesAny)
				if len(intersection) == 0 {
					// There are no `includesAny` keywords in info.Keywords, so this rule does not apply
					//fmt.Println("Excluding due to `includesAny`")
					continue
				}
				//fmt.Println("`includesAny` condition is met")
				processRules = true
			}

			if processRules {
				//fmt.Println("Will process rules")
				//fmt.Printf("Applicable keywords: %s\n", strings.Join(intersection, ", "))
				if rule.Action.Delete {
					keywordsToRemove = append(keywordsToRemove, intersection...)
				}
				if len(rule.Action.Albums) > 0 {
					for _, album := range rule.Action.Albums {
						albumsToAddTo = append(albumsToAddTo, album)
					}
				}
			}
		}
	}

	// Set the keywords to be added to the Flickr photo record
	if len(keywordsToRemove) > 0 {
		difference := Difference(info.Keywords, keywordsToRemove)
		keywordsToAdd = difference
	} else {
		keywordsToAdd = info.Keywords
	}

	// output what we are going to do
	if len(keywordsToRemove) > 0 || len(albumsToAddTo) > 0 {
		fmt.Printf("Actions:\n")
		if len(keywordsToRemove) > 0 {
			fmt.Printf("  - keywords to remove: %s\n", strings.Join(keywordsToRemove, ", "))
		}
		if len(albumsToAddTo) > 0 {
			strs := make([]string, len(albumsToAddTo))
			for i, a := range albumsToAddTo {
				strs[i] = a.Name
			}
			fmt.Printf("  - albums to add to: %s\n", strings.Join(strs, ", "))
		}
		fmt.Printf("\n")
	}

	// All ready to process now
	if dryRun {
		fmt.Println("Would upload photo to Flickr")
		return ""
	}

	if len(keywordsToRemove) > 0 && exiftool != "" {
		// Format of command: exiftool -overwrite_original -keywords-=one -keywords-=two FILENAME
		var parameters []string
		parameters = append(parameters, "-overwrite_original")
		for _, k := range keywordsToRemove {
			parameters = append(parameters, fmt.Sprintf("-keywords-=%s", k))
			parameters = append(parameters, fmt.Sprintf("-subject-=%s", k))
		}
		parameters = append(parameters, filename)
		//fmt.Println("Removing keywords from photo")
		cmd := exec.Command(exiftool, parameters...)
		cmd.Dir = filepath.Dir(filename)
		if err := cmd.Run(); err != nil {
			fmt.Println("Error: ", err)
		}
	}

	// Upload file to Flickr
	fmt.Println("Uploading photo to Flickr")

	client := flickr.NewFlickrClient(apiKey, apiSecret)
	client.OAuthToken = oauthToken
	client.OAuthTokenSecret = oauthTokenSecret

	title := strings.Trim(info.Title, " ")
	if title == "" {
		// no title - use filename (without extension)
		title = filepath.Base(filename)
		title = strings.TrimSuffix(title, filepath.Ext(filename))
	}

	// Upload photo

	// quote keywords for Flickr's tags
	tags := make([]string, len(keywordsToAdd))
	for i, kw := range keywordsToAdd {
		tags[i] = fmt.Sprintf("\"%s\"", kw)
	}

	params := flickr.UploadParams{
		Title:       title,
		Tags:        tags,
		IsPublic:    true,
		IsFamily:    true,
		IsFriend:    true,
		ContentType: 1, // photo
		Hidden:      1, // not hidden
		SafetyLevel: 1, // safe
	}
	if info.Description != "" {
		params.Description = info.Description
	}

	response, err := flickr.UploadFile(client, filename, &params)
	if err != nil {
		fmt.Println(err)
		return ""
	}
	photoId := response.ID
	recordUpload(filename, photoId, config.Upload.StoreUploadListInImageDir)
	fmt.Printf("Uploaded photo '%s'\n", title)

	// set date posted to the date that the photo was taken so that it's in the right place
	// in the Flickr photo stream
	setDatePosted := config.Upload.SetDatePosted
	if setDatePosted == true && info.Date != nil {
		datePosted := fmt.Sprintf("%d", info.Date.Unix())
		respSetDate, err := photos.SetDates(client, photoId, datePosted, "")
		if err != nil {
			fmt.Printf("Failed update photo %v's date posted: %v\n%v\n", photoId, err, respSetDate.ErrorMsg())
		}
	}

	if len(albumsToAddTo) > 0 {
		// assign photo to each photoset in the list
		for _, album := range albumsToAddTo {
			respAdd, err := photosets.AddPhoto(client, album.Id, photoId)
			if err != nil {
				//noinspection GoNilness
				fmt.Println("Failed adding photo to the set: "+album.String(), err, respAdd.ErrorMsg())
			} else {
				fmt.Println("Added photo", photoId, "to set", album.String())
			}
		}
	}

	fmt.Printf("View this photo: http://www.flickr.com/photos/%s/%s\n", config.Flickr.Username, photoId)
	fmt.Println("")
	return photoId
}

func getUploadedListFilename(imageFilename string, storeUploadListInImageDirectory bool) string {
	var directory string

	if storeUploadListInImageDirectory {
		// File is stored in directory where image is and is hidden via a leading `.` on the imageFilename
		directory = filepath.Dir(imageFilename)
		return directory + "/." + uploadedListBaseFilename;
	}

	// Storing to the config directory
	return ConfigDir() + "/" + uploadedListBaseFilename;
}

// Has this file been uploaded to Flickr?
// Check the `.rodeo-uploaded-files` file that resides in the same directory as `filename`
func getUploadedPhotoId(filename string, storeUploadedListInImageDirectory bool) string {
	uploadedListFilename := getUploadedListFilename(filename, storeUploadedListInImageDirectory)
	filenames := readUploadedListFile(uploadedListFilename)

	// Is imageFilename a key in the map?
	imageFilename := filepath.Base(filename)
	if photoId, ok := filenames[imageFilename]; ok {
		// imageFilename exists, return its associated photoId
		return photoId
	}

	return ""
}

// Record the filename of the image uploaded into the uploaded list
func recordUpload(filename string, photoId string, storeUploadedListInImageDirectory bool) {
	imageFilename := filepath.Base(filename)
	uploadedListFilename := getUploadedListFilename(filename, storeUploadedListInImageDirectory)
	filenames := readUploadedListFile(uploadedListFilename)

	// If the imageFilename is already recorded, then there's nothing to do
	if _, ok := filenames[imageFilename]; ok {
		return
	}

	// Filename not in list, so append to list and save
	filenames[imageFilename] = photoId
	writeUploadedListFile(filenames, uploadedListFilename)
}

// Read the uploaded list from the `uploadedListFilename` and convert to a map from the JSON
func readUploadedListFile(uploadedListFilename string) map[string]string {
	filenames := make(map[string]string)

	// Does the file exist?
	if _, err := os.Stat(uploadedListFilename); err == nil || os.IsExist(err) {
		// File exists - therefore read it
		data, err := ioutil.ReadFile(uploadedListFilename)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return filenames
		}

		err = json.Unmarshal(data, &filenames)
		if err != nil {
			fmt.Println("error:", err)
		}
	}

	return filenames
}

// Write the uploaded list to the `uploadedListFilename` in JSON format
func writeUploadedListFile(filenames map[string]string, uploadedListFilename string) {
	// Convert to JSON
	data, err := json.MarshalIndent(filenames, "", "  ")
	if err != nil {
		fmt.Println("error:", err)
	}

	// Write to disk
	err = ioutil.WriteFile(uploadedListFilename, data, 0664)
	if err != nil {
		fmt.Printf("Error: Unable to write %s: %v", filepath.Base(uploadedListFilename), err)
		return
	}
}
