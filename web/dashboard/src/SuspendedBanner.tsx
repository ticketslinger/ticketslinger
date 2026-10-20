export const suspendedText = (organizer: string) =>
  `We've suspended ${organizer}. Your events are off sale and nothing here can be changed, but all your data is still here. Want to talk about it? Email `;

/** Every page of a suspended organizer, for its members. Not dismissible. */
export function SuspendedBanner({ organizer }: { organizer: string }) {
  return (
    <div className="suspended-banner" role="alert">
      <p>
        {suspendedText(organizer)}
        <a href="mailto:support@ticketslinger.com">support@ticketslinger.com</a>.
      </p>
    </div>
  );
}
