// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package actiance_export

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/mattermost/mattermost/server/v8/enterprise/message_export/common_export"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

const (
	XMLNS                   = "http://www.w3.org/2001/XMLSchema-instance"
	ActianceExportFilename  = "actiance_export.xml"
	ActianceWarningFilename = "warning.txt"
)

// The root-level element of an actiance export
type RootNode struct {
	XMLName  xml.Name        `xml:"FileDump"`
	XMLNS    string          `xml:"xmlns:xsi,attr"` // this should default to "http://www.w3.org/2001/XMLSchema-instance"
	Channels []ChannelExport // one element per channel (open or invite-only), group message, or direct message
}

// The Conversation element indicates an ad hoc IM conversation or a group chat room.
// The messages from a persistent chat room are exported once a day so that a Conversation entry contains the messages posted to a chat room from 12:00:00 AM to 11:59:59 PM
type ChannelExport struct {
	XMLName      xml.Name     `xml:"Conversation"`
	Perspective  string       `xml:"Perspective,attr"` // the value of this attribute doesn't seem to matter. Using the channel name makes the export more human readable
	ChannelId    string       `xml:"-"`                // the unique id of the channel
	RoomId       string       `xml:"RoomID"`
	StartTime    int64        `xml:"StartTimeUTC"` // utc timestamp (seconds), start of export period or create time of channel, whichever is greater. Example: 1366611728.
	JoinEvents   []JoinExport // start with a list of all users who were present in the channel during the export period
	Elements     []any
	UploadStarts []*FileUploadStartExport
	UploadStops  []*FileUploadStopExport
	LeaveEvents  []LeaveExport // finish with a list of all users who were present in the channel during the export period
	EndTime      int64         `xml:"EndTimeUTC"` // utc timestamp (seconds), end of export period or delete time of channel, whichever is lesser. Example: 1366611728.
}

// The ParticipantEntered element indicates each user who participates in a conversation.
// For chat rooms, there must be one ParticipantEntered element for each user present in the chat room at the beginning of the reporting period
type JoinExport struct {
	XMLName          xml.Name `xml:"ParticipantEntered"`
	UserEmail        string   `xml:"LoginName"`   // the email of the person that joined the channel
	UserType         string   `xml:"UserType"`    // the type of the user that joined the channel
	JoinTime         int64    `xml:"DateTimeUTC"` // utc timestamp (seconds), time at which the user joined. Example: 1366611728
	CorporateEmailID string   `xml:"CorporateEmailID"`
}

// The ParticipantLeft element indicates the user who leaves an active IM or chat room conversation.
// For chat rooms, there must be one ParticipantLeft element for each user present in the chat room at the end of the reporting period.
type LeaveExport struct {
	XMLName          xml.Name `xml:"ParticipantLeft"`
	UserEmail        string   `xml:"LoginName"`   // the email of the person that left the channel
	UserType         string   `xml:"UserType"`    // the type of the user that left the channel
	LeaveTime        int64    `xml:"DateTimeUTC"` // utc timestamp (seconds), time at which the user left. Example: 1366611728
	CorporateEmailID string   `xml:"CorporateEmailID"`
}

// The Message element indicates the message sent by a user
type PostExport struct {
	XMLName      xml.Name `xml:"Message"`
	UserEmail    string   `xml:"LoginName"`    // the email of the person that sent the post
	UserType     string   `xml:"UserType"`     // the type of the person that sent the post
	PostTime     int64    `xml:"DateTimeUTC"`  // utc timestamp (seconds), time at which the user sent the post. Example: 1366611728
	Message      string   `xml:"Content"`      // the text body of the post
	PreviewsPost string   `xml:"PreviewsPost"` // the post id of the post that is previewed by the permalink preview feature
}

// The FileTransferStarted element indicates the beginning of a file transfer in a conversation
type FileUploadStartExport struct {
	XMLName         xml.Name `xml:"FileTransferStarted"`
	UserEmail       string   `xml:"LoginName"`    // the email of the person that sent the file
	UploadStartTime int64    `xml:"DateTimeUTC"`  // utc timestamp (seconds), time at which the user started the upload. Example: 1366611728
	Filename        string   `xml:"UserFileName"` // the name of the file that was uploaded
	FilePath        string   `xml:"FileName"`     // the path to the file, as stored on the server
}

// The FileTransferEnded element indicates the end of a file transfer in a conversation
type FileUploadStopExport struct {
	XMLName        xml.Name `xml:"FileTransferEnded"`
	UserEmail      string   `xml:"LoginName"`    // the email of the person that sent the file
	UploadStopTime int64    `xml:"DateTimeUTC"`  // utc timestamp (seconds), time at which the user finished the upload. Example: 1366611728
	Filename       string   `xml:"UserFileName"` // the name of the file that was uploaded
	FilePath       string   `xml:"FileName"`     // the path to the file, as stored on the server
	Status         string   `xml:"Status"`       // set to either "Completed" or "Failed" depending on the outcome of the upload operation
}

func ActianceExport(rctx request.CTX, posts []*model.MessageExport, db store.Store, exportBackend filestore.FileBackend, fileAttachmentBackend filestore.FileBackend, exportDirectory string) (warningCount int64, appErr *model.AppError) {
	// sort the posts into buckets based on the channel in which they appeared
	membersByChannel := common_export.MembersByChannel{}
	metadata := common_export.Metadata{
		Channels:         map[string]common_export.MetadataChannel{},
		MessagesCount:    0,
		AttachmentsCount: 0,
		StartTime:        0,
		EndTime:          0,
	}
	elementsByChannel := map[string][]any{}
	allUploadedFiles := []*model.FileInfo{}

	for _, post := range posts {
		if post == nil {
			rctx.Logger().Warn("ignored a nil post reference in the list")
			continue
		}
		elementsByChannel[*post.ChannelId] = append(elementsByChannel[*post.ChannelId], postToExportEntry(post, post.PostCreateAt, *post.PostMessage))

		if post.PostDeleteAt != nil && *post.PostDeleteAt > 0 && post.PostProps != nil {
			props := map[string]any{}
			if json.Unmarshal([]byte(*post.PostProps), &props) == nil {
				if _, ok := props[model.PostPropsDeleteBy]; ok {
					elementsByChannel[*post.ChannelId] = append(elementsByChannel[*post.ChannelId], postToExportEntry(post,
						post.PostDeleteAt, "delete "+*post.PostMessage))
				}
			}
		}

		startUploads, stopUploads, uploadedFiles, deleteFileMessages, err := postToAttachmentsEntries(post, db)
		if err != nil {
			return warningCount, err
		}
		elementsByChannel[*post.ChannelId] = append(elementsByChannel[*post.ChannelId], startUploads...)
		elementsByChannel[*post.ChannelId] = append(elementsByChannel[*post.ChannelId], stopUploads...)
		elementsByChannel[*post.ChannelId] = append(elementsByChannel[*post.ChannelId], deleteFileMessages...)

		allUploadedFiles = append(allUploadedFiles, uploadedFiles...)

		metadata.Update(post, len(uploadedFiles))

		if _, ok := membersByChannel[*post.ChannelId]; !ok {
			membersByChannel[*post.ChannelId] = common_export.ChannelMembers{}
		}
		membersByChannel[*post.ChannelId][*post.UserId] = common_export.ChannelMember{
			Email:    *post.UserEmail,
			UserId:   *post.UserId,
			IsBot:    post.IsBot,
			Username: *post.Username,
		}
	}

	rctx.Logger().Info("Exported data for channels", mlog.Int("number_of_channels", len(metadata.Channels)))

	channelExports := []ChannelExport{}
	for _, channel := range metadata.Channels {
		channelExport, err := buildChannelExport(
			channel,
			membersByChannel[channel.ChannelId],
			elementsByChannel[channel.ChannelId],
			db,
		)
		if err != nil {
			return warningCount, err
		}
		channelExports = append(channelExports, *channelExport)
	}

	export := &RootNode{
		XMLNS:    XMLNS,
		Channels: channelExports,
	}

	return writeExport(rctx, export, allUploadedFiles, exportDirectory, exportBackend, fileAttachmentBackend)
}

func postToExportEntry(post *model.MessageExport, createTime *int64, message string) *PostExport {
	userType := "user"
	if post.IsBot {
		userType = "bot"
	}
	return &PostExport{
		PostTime:     *createTime,
		Message:      message,
		UserType:     userType,
		UserEmail:    *post.UserEmail,
		PreviewsPost: post.PreviewID(),
	}
}

func postToAttachmentsEntries(post *model.MessageExport, db store.Store) ([]any, []any, []*model.FileInfo, []any, *model.AppError) {
	// if the post included any files, we need to add special elements to the export.
	if len(post.PostFileIds) == 0 {
		return nil, nil, nil, nil, nil
	}

	fileInfos, err := db.FileInfo().GetForPost(*post.PostId, true, true, false)
	if err != nil {
		return nil, nil, nil, nil, model.NewAppError("postToAttachmentsEntries", "ent.message_export.actiance_export.get_attachment_error", nil, "", http.StatusInternalServerError).Wrap(err)
	}

	startUploads := []any{}
	stopUploads := []any{}
	deleteFileMessages := []any{}

	uploadedFiles := []*model.FileInfo{}
	for _, fileInfo := range fileInfos {
		// insert a record of the file upload into the export file
		// path to exported file is relative to the xml file, so it's just the name of the exported file
		startUploads = append(startUploads, &FileUploadStartExport{
			UserEmail:       *post.UserEmail,
			Filename:        fileInfo.Name,
			FilePath:        fileInfo.Path,
			UploadStartTime: *post.PostCreateAt,
		})

		stopUploads = append(stopUploads, &FileUploadStopExport{
			UserEmail:      *post.UserEmail,
			Filename:       fileInfo.Name,
			FilePath:       fileInfo.Path,
			UploadStopTime: *post.PostCreateAt,
			Status:         "Completed",
		})

		if fileInfo.DeleteAt > 0 && post.PostDeleteAt != nil {
			deleteFileMessages = append(deleteFileMessages, postToExportEntry(post, post.PostDeleteAt, "delete "+fileInfo.Path))
		}

		uploadedFiles = append(uploadedFiles, fileInfo)
	}
	return startUploads, stopUploads, uploadedFiles, deleteFileMessages, nil
}

func buildChannelExport(channel common_export.MetadataChannel, members common_export.ChannelMembers, elements []any, db store.Store) (*ChannelExport, *model.AppError) {
	channelExport := ChannelExport{
		ChannelId:   channel.ChannelId,
		RoomId:      fmt.Sprintf("%v - %v - %v", common_export.ChannelTypeDisplayName(channel.ChannelType), channel.ChannelName, channel.ChannelId),
		StartTime:   channel.StartTime,
		EndTime:     channel.EndTime,
		Perspective: channel.ChannelDisplayName,
	}

	channelMembersHistory, err := db.ChannelMemberHistory().GetUsersInChannelDuring(channel.StartTime, channel.EndTime, channel.ChannelId)
	if err != nil {
		return nil, model.NewAppError("buildChannelExport", "ent.get_users_in_channel_during", nil, "", http.StatusInternalServerError).Wrap(err)
	}

	joins, leaves := common_export.GetJoinsAndLeavesForChannel(channel.StartTime, channel.EndTime, channelMembersHistory, members)
	type StillJoinedInfo struct {
		Time int64
		Type string
	}
	stillJoined := map[string]StillJoinedInfo{}
	for _, join := range joins {
		userType := "user"
		if join.IsBot {
			userType = "bot"
		}
		channelExport.JoinEvents = append(channelExport.JoinEvents, JoinExport{
			JoinTime:         join.Datetime,
			UserEmail:        join.Email,
			UserType:         userType,
			CorporateEmailID: join.Email,
		})
		if value, ok := stillJoined[join.Email]; !ok {
			stillJoined[join.Email] = StillJoinedInfo{Time: join.Datetime, Type: userType}
		} else {
			if join.Datetime > value.Time {
				stillJoined[join.Email] = StillJoinedInfo{Time: join.Datetime, Type: userType}
			}
		}
	}
	for _, leave := range leaves {
		userType := "user"
		if leave.IsBot {
			userType = "bot"
		}
		channelExport.LeaveEvents = append(channelExport.LeaveEvents, LeaveExport{
			LeaveTime:        leave.Datetime,
			UserEmail:        leave.Email,
			UserType:         userType,
			CorporateEmailID: leave.Email,
		})
		if leave.Datetime > stillJoined[leave.Email].Time {
			delete(stillJoined, leave.Email)
		}
	}

	for email := range stillJoined {
		channelExport.LeaveEvents = append(channelExport.LeaveEvents, LeaveExport{
			LeaveTime:        channel.EndTime,
			UserEmail:        email,
			UserType:         stillJoined[email].Type,
			CorporateEmailID: email,
		})
	}

	sort.Slice(channelExport.LeaveEvents, func(i, j int) bool {
		if channelExport.LeaveEvents[i].LeaveTime == channelExport.LeaveEvents[j].LeaveTime {
			return channelExport.LeaveEvents[i].UserEmail < channelExport.LeaveEvents[j].UserEmail
		}
		return channelExport.LeaveEvents[i].LeaveTime < channelExport.LeaveEvents[j].LeaveTime
	})

	channelExport.Elements = elements
	return &channelExport, nil
}

func writeExport(rctx request.CTX, export *RootNode, uploadedFiles []*model.FileInfo, exportDirectory string, exportBackend filestore.FileBackend, fileAttachmentBackend filestore.FileBackend) (warningCount int64, appErr *model.AppError) {
	// marshal the export object to xml
	xmlData := &bytes.Buffer{}
	xmlData.WriteString(xml.Header)

	enc := xml.NewEncoder(xmlData)
	enc.Indent("", "  ")
	if err := enc.Encode(export); err != nil {
		return warningCount, model.NewAppError("ActianceExport.AtianceExport", "ent.actiance.export.marshalToXml.appError", nil, "", 0).Wrap(err)
	}
	enc.Flush()

	// Try to disable the write timeout if the backend supports it
	if _, err := filestore.TryWriteFileContext(rctx.Context(), exportBackend, xmlData, path.Join(exportDirectory, ActianceExportFilename)); err != nil {
		return warningCount, model.NewAppError("ActianceExport.AtianceExport", "ent.actiance.export.write_file.appError", nil, "", 0).Wrap(err)
	}

	var missingFiles []string
	for _, fileInfo := range uploadedFiles {
		var attachmentSrc io.ReadCloser
		attachmentSrc, nErr := fileAttachmentBackend.Reader(fileInfo.Path)
		if nErr != nil {
			missingFiles = append(missingFiles, "Warning:"+common_export.MissingFileMessage+" - "+fileInfo.Path)
			rctx.Logger().Warn(common_export.MissingFileMessage, mlog.String("FileName", fileInfo.Path))
			continue
		}
		defer attachmentSrc.Close()

		destPath := path.Join(exportDirectory, fileInfo.Path)

		_, nErr = exportBackend.WriteFile(attachmentSrc, destPath)
		if nErr != nil {
			return warningCount, model.NewAppError("ActianceExport.AtianceExport", "ent.actiance.export.write_file.appError", nil, "", 0).Wrap(nErr)
		}
	}
	warningCount = int64(len(missingFiles))
	if warningCount > 0 {
		_, err := filestore.TryWriteFileContext(rctx.Context(), exportBackend, strings.NewReader(strings.Join(missingFiles, "\n")), path.Join(exportDirectory, ActianceWarningFilename))
		if err != nil {
			appErr = model.NewAppError("ActianceExport.AtianceExport", "ent.actiance.export.write_file.appError", nil, "", 0).Wrap(err)
		}
	}
	return warningCount, appErr
}
