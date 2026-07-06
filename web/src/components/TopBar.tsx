import { createContext, useContext, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

import ThemeToggle from '#/components/ThemeToggle'
import { SidebarTrigger } from '#/components/ui/sidebar'

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
export function PageHeader({ title, actions }: { title: ReactNode; actions?: ReactNode }) {
  const slot = useContext(SlotContext)
  if (!slot) return null
  return createPortal(
    <>
      <h1 className="flex min-w-0 items-center truncate text-base font-semibold">{title}</h1>
      {actions ? <div className="ml-auto flex items-center gap-2">{actions}</div> : null}
    </>,
    slot,
  )
}
