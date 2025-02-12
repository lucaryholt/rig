package group

import (
	"context"
	"fmt"
	"strings"

	"github.com/bufbuild/connect-go"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/rigdev/rig-go-api/api/v1/group"
	"github.com/rigdev/rig-go-api/model"
	"github.com/rigdev/rig-go-sdk"
	"github.com/rigdev/rig/cmd/rig/cmd/utils"
	"github.com/spf13/cobra"
)

func GroupList(ctx context.Context, cmd *cobra.Command, args []string, nc rig.Client) error {
	search := strings.Join(args, " ")
	req := &group.ListRequest{
		Pagination: &model.Pagination{
			Offset: uint32(offset),
			Limit:  uint32(limit),
		},
		Search: search,
	}
	resp, err := nc.Group().List(ctx, &connect.Request[group.ListRequest]{Msg: req})
	if err != nil {
		return err
	}

	if outputJSON {
		for _, u := range resp.Msg.GetGroups() {
			cmd.Println(utils.ProtoToPrettyJson(u))
		}
		return nil
	}

	t := table.NewWriter()
	t.AppendHeader(table.Row{fmt.Sprintf("Groups (%d)", resp.Msg.GetTotal()), "Name", "ID"})
	for i, g := range resp.Msg.GetGroups() {
		t.AppendRow(table.Row{i + 1, g.GetName(), g.GetGroupId()})
	}
	cmd.Println(t.Render())
	return nil
}
