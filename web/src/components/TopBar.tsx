import { createContext, useContext, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { HelpCircle } from 'lucide-react'

import ThemeToggle from '#/components/ThemeToggle'
import { SidebarTrigger } from '#/components/ui/sidebar'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '#/components/ui/tooltip'

// The app's single top bar. It hosts the sidebar toggle + theme toggle and
// exposes a slot (via context) that each page fills with its title and primary
// action — so there's no second header row wasting vertical space. Pages render
// <PageHeader> and it portals into this slot while keeping its React state
// (dialogs, mutations) inside the page tree.
const SlotContext = createContext<HTMLElement | null>(null)

export function TopBar({ children }: { children: ReactNode }) {
  const [slot, setSlot] = useState<HTMLElement | null>(null)
  return (
    <>
      <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border px-4">
        <SidebarTrigger className="-ml-1" />
        <div ref={setSlot} className="flex flex-1 items-center gap-3" />
        <ThemeToggle />
      </header>
      <SlotContext.Provider value={slot}>{children}</SlotContext.Provider>
    </>
  )
}

// PageHeader renders a page's title (and optional right-aligned actions) into
// the top bar. Returns null until the slot mounts (and during SSR) — the title
// then appears after hydration.
export function PageHeader({
  title,
  help,
  actions,
}: {
  title: ReactNode
  // Short blurb of what the page is for, shown in a tooltip behind a help icon.
  help?: ReactNode
  actions?: ReactNode
}) {
  const slot = useContext(SlotContext)
  if (!slot) return null
  return createPortal(
    <>
      <h1 className="flex min-w-0 items-center gap-1.5 text-base font-semibold">
        <span className="truncate">{title}</span>
        {help ? (
          <TooltipProvider>
            <Tooltip>
              <TooltipTrigger asChild>
                <button
                  type="button"
                  aria-label="About this page"
                  className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
                >
                  <HelpCircle className="h-4 w-4" />
                </button>
              </TooltipTrigger>
              <TooltipContent className="max-w-xs text-pretty">{help}</TooltipContent>
            </Tooltip>
          </TooltipProvider>
        ) : null}
      </h1>
      {actions ? <div className="ml-auto flex items-center gap-2">{actions}</div> : null}
    </>,
    slot,
  )
}
