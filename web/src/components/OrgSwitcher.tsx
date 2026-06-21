import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronsUpDown, Plus, Check } from 'lucide-react'
import { Link } from '@tanstack/react-router'

import { api, qk } from '#/lib/api'
import { Button } from '#/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from '#/components/ui/dropdown-menu'

export function OrgSwitcher() {
  const qc = useQueryClient()
  const { data: me } = useQuery({ queryKey: qk.me(), queryFn: api.me })

  const select = useMutation({
    mutationFn: api.selectOrg,
    // selectOrg rotates the Set-Cookie carrying active_org_id. We must
    // (a) wait for axios to commit the new cookie to the browser jar, then
    // (b) force a fresh `me` fetch so every consumer of qk.me() observes
    // the new active org before we evict any other queries. Only then do
    // we drop the stale org-scoped caches — invalidate-all would race the
    // cookie commit and refetch with the old org id, repainting the UI
    // with the previous tenant's data for one frame.
    onSuccess: async () => {
      await qc.refetchQueries({ queryKey: qk.me() })
      qc.removeQueries({
        predicate: (q) => {
          const k = q.queryKey[0]
          return k !== 'me'
        },
      })
    },
  })

  if (!me?.user) return null

  const active = me.orgs?.find((o) => o.id === me.active_org_id)

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="sm" className="gap-2">
          <span className="h-2 w-2 rounded-full bg-[linear-gradient(90deg,#56c6be,#7ed3bf)]" />
          <span className="max-w-[10rem] truncate">{active?.name ?? 'Choose org'}</span>
          <ChevronsUpDown className="h-3 w-3 opacity-50" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent className="w-56">
        {me.orgs?.map((o) => (
          <DropdownMenuItem
            key={o.id}
            onClick={() => select.mutate(o.id)}
            disabled={select.isPending}
          >
            <span className="flex-1 truncate">{o.name}</span>
            {o.id === me.active_org_id && <Check className="h-4 w-4" />}
          </DropdownMenuItem>
        ))}
        {(me.orgs?.length ?? 0) > 0 && <DropdownMenuSeparator />}
        <DropdownMenuItem asChild>
          <Link to="/orgs/new" className="flex items-center gap-2">
            <Plus className="h-4 w-4" /> Create org
          </Link>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
