package milter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-milter"
	"github.com/foxcpp/maddy/internal/buffer"
	"github.com/foxcpp/maddy/internal/config"
	"github.com/foxcpp/maddy/internal/exterrors"
	"github.com/foxcpp/maddy/internal/log"
	"github.com/foxcpp/maddy/internal/module"
	"github.com/foxcpp/maddy/internal/target"
)

const modName = "milter"

type Check struct {
	cl        *milter.Client
	milterUrl string
	failOpen  bool
	instName  string
	log       log.Logger
}

func New(_, instName string, _, inlineArgs []string) (module.Module, error) {
	c := &Check{
		instName: instName,
		log:      log.Logger{Name: modName, Debug: log.DefaultLogger.Debug},
	}
	switch len(inlineArgs) {
	case 1:
		c.milterUrl = inlineArgs[0]
	case 0:
	default:
		return nil, fmt.Errorf("%s: unexpected amount of arguments, want 1 or 0", modName)
	}
	return c, nil
}

func (c *Check) Name() string {
	return modName
}

func (c *Check) InstanceName() string {
	return c.instName
}

func (c *Check) Init(cfg *config.Map) error {
	cfg.String("endpoint", false, false, c.milterUrl, &c.milterUrl)
	cfg.Bool("fail_open", false, false, &c.failOpen)
	if _, err := cfg.Process(); err != nil {
		return err
	}

	if c.milterUrl == "" {
		return fmt.Errorf("%s: milter endpoint is not set", modName)
	}

	endp, err := config.ParseEndpoint(c.milterUrl)
	if err != nil {
		return fmt.Errorf("%s: %v", modName, err)
	}

	switch endp.Scheme {
	case "tcp", "unix":
	default:
		return fmt.Errorf("%s: scheme unsupported: %v", modName, endp.Scheme)
	}
	if endp.Path != "" {
		return fmt.Errorf("%s: stray path in endpoint: %v", modName, endp)
	}

	c.cl = milter.NewClient(endp.Scheme, endp.Host)

	return nil
}

type state struct {
	c          *Check
	session    *milter.ClientSession
	msgMeta    *module.MsgMetadata
	skipChecks bool
	log        log.Logger
}

func (c *Check) CheckStateForMsg(msgMeta *module.MsgMetadata) (module.CheckState, error) {
	const supportedActions = milter.OptAddHeader | milter.OptQuarantine
	protocolOpts := milter.OptProtocol(0)
	if msgMeta.Conn == nil {
		protocolOpts = milter.OptNoConnect | milter.OptNoHelo
	}

	session, err := c.cl.Session(supportedActions, protocolOpts)
	if err != nil {
		return nil, err
	}
	return &state{
		c:       c,
		session: session,
		msgMeta: msgMeta,
		log:     target.DeliveryLogger(c.log, msgMeta),
	}, nil
}

func (s *state) handleAction(act *milter.Action) module.CheckResult {
	switch act.Code {
	case milter.ActAccept:
		s.skipChecks = true
		return module.CheckResult{}
	case milter.ActContinue:
		return module.CheckResult{}
	case milter.ActReplyCode:
		return module.CheckResult{
			Reject: true,
			Reason: &exterrors.SMTPError{
				Code:         act.SMTPCode,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 1},
				Message:      "Message rejected due to local policy",
				Reason:       "reply code action",
				CheckName:    "milter",
				Misc: map[string]interface{}{
					"milter": s.c.milterUrl,
				},
			},
		}
	case milter.ActDiscard:
		s.log.Msg("silent discard is not supported, rejecting message")
		fallthrough
	case milter.ActReject:
		return module.CheckResult{
			Reject: true,
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 1},
				Message:      "Message rejected due to local policy",
				Reason:       "reject action",
				CheckName:    "milter",
				Misc: map[string]interface{}{
					"milter": s.c.milterUrl,
				},
			},
		}
	default:
		s.log.Msg("unknown action code ignored", "code", act.Code, "milter", s.c.milterUrl)
		return module.CheckResult{}
	}
}

// apply applies the modification actions returned by milter to the check results object.
func (s *state) apply(modifyActs []milter.ModifyAction, res module.CheckResult) module.CheckResult {
	out := res
	for _, act := range modifyActs {
		switch act.Code {
		case milter.ActAddRcpt, milter.ActDelRcpt:
			s.log.Msg("envelope changes are not supported", "rcpt", act.Rcpt, "code", act.Code, "milter", s.c.milterUrl)
		case milter.ActChangeFrom:
			s.log.Msg("envelope changes are not supported", "from", act.From, "code", act.Code, "milter", s.c.milterUrl)
		case milter.ActChangeHeader:
			s.log.Msg("header field changes are not supported", "field", act.HdrName, "milter", s.c.milterUrl)
		case milter.ActInsertHeader:
			if act.HdrIndex != 1 {
				s.log.Msg("header inserting not on top is not supported, prepending instead", "field", act.HdrName, "milter", s.c.milterUrl)
			}
			fallthrough
		case milter.ActAddHeader:
			// Header field might be arbitarly folded by the caller and we want
			// to preserve that exact format in case it is important (DKIM
			// signature is added by milter).
			field := make([]byte, 0, len(act.HdrName)+2+len(act.HdrValue)+2)
			field = append(field, act.HdrName...)
			field = append(field, ':', ' ')
			field = append(field, act.HdrValue...)
			field = append(field, '\r', '\n')
			out.Header.AddRaw(field)
		case milter.ActQuarantine:
			out.Quarantine = true
			out.Reason = exterrors.WithFields(errors.New("milter quarantine action"), map[string]interface{}{
				"check":  "milter",
				"milter": s.c.milterUrl,
				"reason": act.Reason,
			})
		}
	}
	return out
}

func (s *state) CheckConnection(ctx context.Context) module.CheckResult {
	if s.msgMeta.Conn == nil {
		return module.CheckResult{}
	}

	if !s.session.ProtocolOption(milter.OptNoConnect) {
		if err := s.session.Macros(milter.CodeConn,
			"daemon_name", "maddy",
			"if_name", "unknown",
			"if_addr", "0.0.0.0",
			// TODO: $j
			// TODO: $_
		); err != nil {
			return s.ioError(err)
		}

		var (
			protoFamily milter.ProtoFamily
			port        uint16
			addr        string
		)
		switch rAddr := s.msgMeta.Conn.RemoteAddr.(type) {
		case *net.TCPAddr:
			port = uint16(rAddr.Port)
			if v4 := rAddr.IP.To4(); v4 != nil {
				// Make sure to not accidentally send IPv6-mapped IPv4 address.
				protoFamily = milter.FamilyInet
				addr = v4.String()
			} else {
				protoFamily = milter.FamilyInet6
				addr = rAddr.IP.String()
			}
		case *net.UnixAddr:
			protoFamily = milter.FamilyUnix
			addr = rAddr.Name
		default:
			protoFamily = milter.FamilyUnknown
		}

		act, err := s.session.Conn(s.msgMeta.Conn.Hostname, protoFamily, port, addr)
		if err != nil {
			return s.ioError(err)
		}
		if act.Code != milter.ActContinue {
			return s.handleAction(act)
		}
	}

	if !s.session.ProtocolOption(milter.OptNoHelo) {
		if s.msgMeta.Conn.TLS.HandshakeComplete {
			fields := make([]string, 0, 4*2)
			tlsState := s.msgMeta.Conn.TLS

			switch tlsState.Version {
			case tls.VersionTLS10:
				fields = append(fields, "tls_version", "TLSv1")
			case tls.VersionTLS11:
				fields = append(fields, "tls_version", "TLSv1.1")
			case tls.VersionTLS12:
				fields = append(fields, "tls_version", "TLSv1.2")
			case tls.VersionTLS13:
				fields = append(fields, "tls_version", "TLSv1.3")
			}
			fields = append(fields, "cipher", tls.CipherSuiteName(tlsState.CipherSuite))

			if len(tlsState.PeerCertificates) != 0 {
				fields = append(fields, "cert_subject",
					tlsState.PeerCertificates[len(tlsState.PeerCertificates)-1].Subject.String())
				fields = append(fields, "cert_issuer",
					tlsState.PeerCertificates[len(tlsState.PeerCertificates)-1].Issuer.String())
			}

			if err := s.session.Macros(milter.CodeHelo, fields...); err != nil {
				return s.ioError(err)
			}
		}
		act, err := s.session.Helo(s.msgMeta.Conn.Hostname)
		if err != nil {
			return s.ioError(err)
		}
		return s.handleAction(act)
	}

	return module.CheckResult{}
}

func (s *state) ioError(err error) module.CheckResult {
	if s.c.failOpen {
		s.skipChecks = true // silently permit processing to continue
		s.c.log.Error("I/O error", err)
		return module.CheckResult{}
	}

	return module.CheckResult{
		Reject: true,
		Reason: &exterrors.SMTPError{
			Code:         451,
			EnhancedCode: exterrors.EnhancedCode{4, 7, 1},
			Message:      "I/O error during policy check",
			Err:          err,
			CheckName:    "milter",
			Misc: map[string]interface{}{
				"milter": s.c.milterUrl,
			},
		},
	}
}

func (s *state) CheckSender(ctx context.Context, mailFrom string) module.CheckResult {
	if s.skipChecks || s.session.ProtocolOption(milter.OptNoMailFrom) {
		return module.CheckResult{}
	}

	fields := make([]string, 0, 2)
	fields = append(fields, "i", s.msgMeta.ID)
	// TODO: fields = append(fields, "auth_type", s.msgMeta.???)
	if s.msgMeta.Conn.AuthUser != "" {
		fields = append(fields, "auth_authen", s.msgMeta.Conn.AuthUser)
	}
	if err := s.session.Macros(milter.CodeMail, fields...); err != nil {
		return s.ioError(err)
	}

	esmtpArgs := make([]string, 0, 2)
	if s.msgMeta.SMTPOpts.UTF8 {
		esmtpArgs = append(esmtpArgs, "SMTPUTF8")
	}

	act, err := s.session.Mail(mailFrom, esmtpArgs)
	if err != nil {
		return s.ioError(err)
	}
	return s.handleAction(act)
}

func (s *state) CheckRcpt(ctx context.Context, rcptTo string) module.CheckResult {
	if s.skipChecks {
		return module.CheckResult{}
	}

	act, err := s.session.Rcpt(rcptTo, nil)
	if err != nil {
		return s.ioError(err)
	}
	return s.handleAction(act)
}

func (s *state) CheckBody(ctx context.Context, header textproto.Header, body buffer.Buffer) module.CheckResult {
	if s.skipChecks {
		return module.CheckResult{}
	}

	act, err := s.session.Header(header)
	if err != nil {
		return s.ioError(err)
	}
	if act.Code != milter.ActContinue {
		return s.handleAction(act)
	}

	var modifyAct []milter.ModifyAction

	if !s.session.ProtocolOption(milter.OptNoBody) {
		// body.Open can be expensive for on-disk buffering.
		r, err := body.Open()
		if err != nil {
			// Not ioError(err) because fail_open directive is applied only for external I/O.
			return module.CheckResult{
				Reject: true,
				Reason: &exterrors.SMTPError{
					Code:         451,
					EnhancedCode: exterrors.EnhancedCode{4, 7, 1},
					Message:      "Internal error during policy check",
					Err:          err,
					CheckName:    "milter",
					Misc: map[string]interface{}{
						"milter": s.c.milterUrl,
					},
				},
			}
		}

		modifyAct, act, err = s.session.Body(r)
		if err != nil {
			return s.ioError(err)
		}
	} else {
		modifyAct, act, err = s.session.End()
		if err != nil {
			return s.ioError(err)
		}
	}

	result := s.handleAction(act)
	return s.apply(modifyAct, result)
}

func (s *state) Close() error {
	return s.session.Close()
}

func init() {
	module.Register(modName, New)
}
