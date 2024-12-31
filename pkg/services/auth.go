package services

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-faster/errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	tgauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/tgdrive/teldrive/internal/api"
	"github.com/tgdrive/teldrive/internal/auth"
	"github.com/tgdrive/teldrive/internal/logging"
	"github.com/tgdrive/teldrive/internal/tgc"
	"github.com/tgdrive/teldrive/pkg/models"
	"github.com/tgdrive/teldrive/pkg/types"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var authCookieName = "access_token"

func (a *apiService) AuthLogin(ctx context.Context, session *api.SessionCreate) (*api.AuthLoginNoContent, error) {

	if !checkUserIsAllowed(a.cnf.JWT.AllowedUsers, session.UserName) {
		return nil, &apiError{code: http.StatusForbidden, err: errors.New("user not allowed")}
	}

	now := time.Now().UTC()

	jwtClaims := &types.JWTClaims{
		Name:      session.Name,
		UserName:  session.UserName,
		IsPremium: session.IsPremium,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(session.UserId, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.cnf.JWT.SessionTime)),
		}}

	tokenhash := md5.Sum([]byte(session.Session))
	hexToken := hex.EncodeToString(tokenhash[:])
	jwtClaims.Hash = hexToken

	jwtToken, err := auth.Encode(a.cnf.JWT.Secret, jwtClaims)

	if err != nil {
		return nil, &apiError{err: err}
	}

	user := models.User{
		UserId:    session.UserId,
		Name:      session.Name,
		UserName:  session.UserName,
		IsPremium: session.IsPremium,
	}

	err = a.db.Transaction(func(tx *gorm.DB) error {

		if err := a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&user).Error; err != nil {
			return err
		}
		file := &models.File{
			Name:     "root",
			Type:     "folder",
			MimeType: "drive/folder",
			UserID:   session.UserId,
			Status:   "active",
			Parts:    nil,
		}
		if err := a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(file).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, &apiError{err: err}
	}

	client, _ := tgc.AuthClient(ctx, &a.cnf.TG, session.Session)

	var auth *tg.Authorization

	err = client.Run(ctx, func(ctx context.Context) error {
		auths, err := client.API().AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		for _, a := range auths.Authorizations {
			if a.Current {
				auth = &a
				break
			}
		}
		return nil
	})

	if err != nil {
		return nil, &apiError{err: err}
	}

	if err := a.db.Create(&models.Session{UserId: session.UserId, Hash: hexToken,
		Session: session.Session, SessionDate: auth.DateCreated}).Error; err != nil {
		return nil, &apiError{err: err}
	}
	return &api.AuthLoginNoContent{SetCookie: setCookie(authCookieName, jwtToken, int(a.cnf.JWT.SessionTime.Seconds()))}, nil
}

func (a *apiService) AuthLogout(ctx context.Context) (*api.AuthLogoutNoContent, error) {
	authUser := auth.GetJWTUser(ctx)
	client, _ := tgc.AuthClient(ctx, &a.cnf.TG, authUser.TgSession)
	tgc.RunWithAuth(ctx, client, "", func(ctx context.Context) error {
		_, err := client.API().AuthLogOut(ctx)
		return err
	})
	a.db.Where("session = ?", authUser.TgSession).Delete(&models.Session{})
	a.cache.Delete(fmt.Sprintf("sessions:%s", authUser.Hash))
	return &api.AuthLogoutNoContent{SetCookie: setCookie(authCookieName, "", -1)}, nil
}

func (a *apiService) AuthSession(ctx context.Context, params api.AuthSessionParams) (api.AuthSessionRes, error) {
	if !params.AccessToken.IsSet() {
		return &api.AuthSessionNoContent{}, nil
	}
	claims, err := auth.VerifyUser(a.db, a.cache, a.cnf.JWT.Secret, params.AccessToken.Value)

	if err != nil {
		return &api.AuthSessionNoContent{}, nil
	}

	claims.TgSession = ""

	now := time.Now().UTC()

	newExpires := now.Add(a.cnf.JWT.SessionTime)

	userId, _ := strconv.ParseInt(claims.Subject, 10, 64)

	session := api.Session{
		Name:     claims.Name,
		UserName: claims.UserName,
		UserId:   userId,
		Hash:     claims.Hash,
		Expires:  newExpires}

	claims.IssuedAt = jwt.NewNumericDate(now)

	claims.ExpiresAt = jwt.NewNumericDate(newExpires)

	jweToken, err := auth.Encode(a.cnf.JWT.Secret, claims)

	if err != nil {
		return &api.AuthSessionNoContent{}, nil
	}
	return &api.SessionHeaders{SetCookie: setCookie(authCookieName, jweToken, int(a.cnf.JWT.SessionTime.Seconds())),
		Response: session}, nil
}

func (a *apiService) AuthWs(ctx context.Context) error {
	return nil
}

func (e *extendedService) AuthWs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	logger := logging.FromContext(ctx).With(zap.String("handler", "AuthWs"))
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("websocket upgrade error", zap.Error(err))
		http.Error(w, "could not upgrade connection", http.StatusBadRequest)
		return
	}

	defer func() {
		if err := conn.Close(); err != nil {
			logger.Error("error closing websocket connection", zap.Error(err))
		} else {
			logger.Info("websocket connection closed")
		}
	}()

	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(dispatcher)
	sessionStorage := &session.StorageMemory{}
	tgClient, err := tgc.NoAuthClient(ctx, &e.api.cnf.TG, dispatcher, sessionStorage)
	if err != nil {
		logger.Error("error creating telegram client", zap.Error(err))
		return
	}

	err = tgClient.Run(ctx, func(ctx context.Context) error {
		for {
			message := &types.SocketMessage{}
			err := conn.ReadJSON(message)
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					logger.Debug("websocket connection closed normally by client")
					return nil
				}
				logger.Error("websocket read error", zap.Error(err))
				return err
			}
			switch message.AuthType {
			case "qr":
				go e.handleQRAuth(ctx, conn, tgClient, loggedIn, sessionStorage, logger)
			case "phone":
				if message.Message == "sendcode" {
					go e.handleSendCode(ctx, conn, tgClient, message, logger)
				} else if message.Message == "signin" {
					go e.handleSignIn(ctx, conn, tgClient, message, sessionStorage, logger)
				}
			case "2fa":
				if message.Password != "" {
					go e.handle2FA(ctx, conn, tgClient, message, sessionStorage, logger)
				}
			}
		}
	})

	if err != nil {
		logger.Error("error during tgClient.Run", zap.Error(err))
		if !errors.Is(err, context.Canceled) {
		}
		return
	}
}

func (e *extendedService) handleQRAuth(
	ctx context.Context,
	conn *websocket.Conn,
	tgClient *telegram.Client,
	loggedIn qrlogin.LoggedIn,
	sessionStorage *session.StorageMemory,
	logger *zap.Logger) {
	logger = logger.With(zap.String("handler", "handleQRAuth"))
	authorization, err := tgClient.QR().Auth(ctx, loggedIn, func(ctx context.Context, token qrlogin.Token) error {
		return conn.WriteJSON(map[string]interface{}{"type": "auth", "payload": map[string]string{"token": token.URL()}})
	})

	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		logger.Debug("2FA required for QR auth")
		conn.WriteJSON(map[string]interface{}{"type": "auth", "message": "2FA required"})
		return
	}

	if err != nil {
		logger.Error("QR auth error", zap.Error(err))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	user, ok := authorization.User.AsNotEmpty()
	if !ok {
		logger.Error("QR auth failed, user not found")
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "auth failed"})
		return
	}
	if !checkUserIsAllowed(e.api.cnf.JWT.AllowedUsers, user.Username) {
		logger.Error("user not allowed", zap.String("username", user.Username))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "user not allowed"})
		tgClient.API().AuthLogOut(ctx)
		return
	}

	res, _ := sessionStorage.LoadSession(ctx)
	sessionData := &types.SessionData{}
	json.Unmarshal(res, sessionData)
	session := prepareSession(user, &sessionData.Data)
	conn.WriteJSON(map[string]interface{}{"type": "auth", "payload": session, "message": "success"})
	logger.Info("QR auth success", zap.String("username", user.Username))
}

func (e *extendedService) handleSendCode(
	ctx context.Context,
	conn *websocket.Conn,
	tgClient *telegram.Client,
	message *types.SocketMessage,
	logger *zap.Logger) {
	logger = logger.With(zap.String("handler", "handleSendCode"))

	res, err := tgClient.Auth().SendCode(ctx, message.PhoneNo, tgauth.SendCodeOptions{})
	if err != nil {
		logger.Error("error sending code", zap.Error(err), zap.String("phoneNo", message.PhoneNo))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	code := res.(*tg.AuthSentCode)
	conn.WriteJSON(map[string]interface{}{"type": "auth", "payload": map[string]string{"phoneCodeHash": code.PhoneCodeHash}})
}

func (e *extendedService) handleSignIn(
	ctx context.Context,
	conn *websocket.Conn,
	tgClient *telegram.Client,
	message *types.SocketMessage,
	sessionStorage *session.StorageMemory,
	logger *zap.Logger) {
	logger = logger.With(zap.String("handler", "handleSignIn"))

	auth, err := tgClient.Auth().SignIn(ctx, message.PhoneNo, message.PhoneCode, message.PhoneCodeHash)

	if errors.Is(err, tgauth.ErrPasswordAuthNeeded) {
		logger.Debug("2FA required for phone sign in")
		conn.WriteJSON(map[string]interface{}{"type": "auth", "message": "2FA required"})
		return
	}

	if err != nil {
		logger.Error("phone sign-in error", zap.Error(err))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	user, ok := auth.User.AsNotEmpty()
	if !ok {
		logger.Error("phone sign-in failed, user not found")
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "auth failed"})
		return
	}
	if !checkUserIsAllowed(e.api.cnf.JWT.AllowedUsers, user.Username) {
		logger.Error("user not allowed", zap.String("username", user.Username))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "user not allowed"})
		tgClient.API().AuthLogOut(ctx)
		return
	}

	res, _ := sessionStorage.LoadSession(ctx)
	sessionData := &types.SessionData{}
	json.Unmarshal(res, sessionData)
	session := prepareSession(user, &sessionData.Data)
	conn.WriteJSON(map[string]interface{}{"type": "auth", "payload": session, "message": "success"})
	logger.Debug("phone sign in success", zap.String("username", user.Username))
}
func (e *extendedService) handle2FA(
	ctx context.Context,
	conn *websocket.Conn,
	tgClient *telegram.Client,
	message *types.SocketMessage,
	sessionStorage *session.StorageMemory,
	logger *zap.Logger) {
	logger = logger.With(zap.String("handler", "handle2FA"))
	auth, err := tgClient.Auth().Password(ctx, message.Password)
	if err != nil {
		logger.Error("2FA authentication error", zap.Error(err))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	user, ok := auth.User.AsNotEmpty()
	if !ok {
		logger.Error("2FA authentication failed, user not found")
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "auth failed"})
		return
	}
	if !checkUserIsAllowed(e.api.cnf.JWT.AllowedUsers, user.Username) {
		logger.Error("user not allowed", zap.String("username", user.Username))
		conn.WriteJSON(map[string]interface{}{"type": "error", "message": "user not allowed"})
		tgClient.API().AuthLogOut(ctx)
		return
	}
	res, _ := sessionStorage.LoadSession(ctx)
	sessionData := &types.SessionData{}
	json.Unmarshal(res, sessionData)
	session := prepareSession(user, &sessionData.Data)
	conn.WriteJSON(map[string]interface{}{"type": "auth", "payload": session, "message": "success"})
	logger.Debug("2FA authentication success", zap.String("username", user.Username))
}

func ip4toInt(IPv4Address net.IP) int64 {
	IPv4Int := big.NewInt(0)
	IPv4Int.SetBytes(IPv4Address.To4())
	return IPv4Int.Int64()
}

func pack32BinaryIP4(ip4Address string) []byte {
	ipv4Decimal := ip4toInt(net.ParseIP(ip4Address))

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint32(ipv4Decimal))
	return buf.Bytes()
}

func generateTgSession(dcID int, authKey []byte, port int) string {

	dcMaps := map[int]string{
		1: "149.154.175.53",
		2: "149.154.167.51",
		3: "149.154.175.100",
		4: "149.154.167.91",
		5: "91.108.56.130",
	}

	dcIDByte := byte(dcID)
	serverAddressBytes := pack32BinaryIP4(dcMaps[dcID])
	portByte := make([]byte, 2)
	binary.BigEndian.PutUint16(portByte, uint16(port))

	packet := make([]byte, 0)
	packet = append(packet, dcIDByte)
	packet = append(packet, serverAddressBytes...)
	packet = append(packet, portByte...)
	packet = append(packet, authKey...)

	base64Encoded := base64.URLEncoding.EncodeToString(packet)
	return "1" + base64Encoded
}

func checkUserIsAllowed(allowedUsers []string, userName string) bool {
	found := false
	if len(allowedUsers) > 0 {
		for _, user := range allowedUsers {
			if user == userName {
				found = true
				break
			}
		}
	} else {
		found = true
	}
	return found
}

func prepareSession(user *tg.User, data *session.Data) *api.SessionCreate {
	sessionString := generateTgSession(data.DC, data.AuthKey, 443)
	session := &api.SessionCreate{
		Session:   sessionString,
		UserId:    user.ID,
		UserName:  user.Username,
		Name:      fmt.Sprintf("%s %s", user.FirstName, user.LastName),
		IsPremium: user.Premium,
	}
	return session
}

func setCookie(name, value string, maxAge int) string {
	cookie := http.Cookie{
		Name:     name,
		Value:    value,
		MaxAge:   maxAge,
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
	}
	return cookie.String()
}
