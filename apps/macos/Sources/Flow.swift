import SwiftUI

/// Flow is the hero: one ribbon per uplink, entering from the left, weaving
/// over and under each other, leaving as a single braided cord.
///
/// It is not decoration. Ribbon thickness is that link's real share of the
/// work, light travels along it at the measured rate, and a link that stops
/// carrying data visibly thins and dims. A glance answers "is bonding working,
/// and which link is doing it" without reading a number.
struct Flow: View {
    let links: [LinkInfo]
    let shares: [String: Double]
    let mbps: Double
    let active: Bool

    var body: some View {
        TimelineView(.animation(minimumInterval: 1.0 / 60.0, paused: false)) { timeline in
            Canvas { context, size in
                let t = timeline.date.timeIntervalSinceReferenceDate
                draw(&context, size: size, time: t)
            }
        }
        .background(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .fill(
                    RadialGradient(
                        colors: [Theme.surface, Theme.ground],
                        center: .center, startRadius: 4, endRadius: 520
                    )
                )
        )
        .overlay(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .strokeBorder(Theme.rule, lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
    }

    private func draw(_ context: inout GraphicsContext, size: CGSize, time: Double) {
        let lanes = links.isEmpty
            ? [LinkInfo(iface: "idle", label: "—", metered: false)]
            : links
        // Idle still breathes, so the panel never looks dead or frozen.
        let rate = active ? max(mbps, 4) : 5
        let travel = time * (0.16 + min(1.1, pow(rate / 130.0, 0.6)))
        let mergeX = size.width * 0.34
        let mid = size.height / 2

        for (i, link) in lanes.enumerated() {
            let share = links.isEmpty ? 0.5 : (shares[link.iface] ?? 0.12)
            let colour = links.isEmpty
                ? Theme.inkFaint
                : Theme.colour(for: link.iface, links: links)

            // Thickness is the link's share, with a floor so an idle link is
            // still visibly present rather than gone.
            let weight = 4.0 + share * 26.0
            let path = strand(size: size, laneIndex: i, laneCount: lanes.count,
                              mergeX: mergeX, mid: mid, time: time)

            // Glow underneath, so the cord reads as light rather than a line.
            context.stroke(path, with: .color(colour.opacity(active ? 0.16 : 0.09)),
                           style: StrokeStyle(lineWidth: weight * 2.6, lineCap: .round))
            context.stroke(path, with: .color(colour.opacity(active ? 0.42 : 0.26)),
                           style: StrokeStyle(lineWidth: weight, lineCap: .round))

            // Light running along the cord. A dashed stroke with a moving phase
            // gives motion far more cheaply than hundreds of particles.
            let dash = StrokeStyle(
                lineWidth: weight * 0.66,
                lineCap: .round,
                dash: [10, 30],
                dashPhase: -travel * 190
            )
            context.stroke(path, with: .color(colour.opacity(active ? 0.95 : 0.4)), style: dash)
        }

        // Where the strands meet, a soft node: the moment of bonding.
        if lanes.count > 1 {
            let glow = Path(ellipseIn: CGRect(x: mergeX - 26, y: mid - 26, width: 52, height: 52))
            context.fill(glow, with: .radialGradient(
                Gradient(colors: [Theme.ink.opacity(active ? 0.20 : 0.10), .clear]),
                center: CGPoint(x: mergeX, y: mid), startRadius: 0, endRadius: 26
            ))
        }
    }

    /// strand is one uplink's path: in from its own lane, curving to the centre,
    /// then weaving. Lanes are in antiphase so they cross over and under.
    private func strand(size: CGSize, laneIndex: Int, laneCount: Int,
                        mergeX: CGFloat, mid: CGFloat, time: Double) -> Path {
        let laneY = laneCentre(laneIndex, laneCount, size.height)
        var path = Path()
        path.move(to: CGPoint(x: -6, y: laneY))
        path.addCurve(
            to: CGPoint(x: mergeX, y: mid),
            control1: CGPoint(x: mergeX * 0.45, y: laneY),
            control2: CGPoint(x: mergeX * 0.72, y: mid)
        )

        // After the merge the strands braid: a slow travelling sine, each lane
        // offset by half a turn so they interleave instead of overlapping.
        let amplitude = size.height * 0.17
        let phase = time * 0.9 + Double(laneIndex) * .pi
        let steps = 90
        for s in 0 ... steps {
            let k = Double(s) / Double(steps)
            let x = mergeX + (size.width - mergeX) * k
            // The weave tightens toward the exit, so the cord resolves into one.
            let taper = 1.0 - k * 0.45
            let y = mid + CGFloat(sin(k * .pi * 2.6 + phase) * Double(amplitude) * taper)
            path.addLine(to: CGPoint(x: x, y: y))
        }
        return path
    }

    private func laneCentre(_ index: Int, _ count: Int, _ height: CGFloat) -> CGFloat {
        guard count > 1 else { return height / 2 }
        let usable = height * 0.66
        let top = (height - usable) / 2
        return top + usable * CGFloat(index) / CGFloat(count - 1)
    }
}

/// Odometer rolls each digit into place, so a rising rate feels like climbing
/// rather than flickering.
struct Odometer: View {
    let value: Double
    var size: CGFloat = 58

    private var text: String { String(format: "%.1f", max(0, value)) }

    var body: some View {
        HStack(spacing: 0) {
            ForEach(Array(text.enumerated()), id: \.offset) { _, ch in
                if ch == "." {
                    Text(".").font(.system(size: size, weight: .semibold))
                        .frame(width: size * 0.24)
                } else {
                    Text(String(ch))
                        .font(.system(size: size, weight: .semibold))
                        .frame(width: size * 0.56)
                        .contentTransition(.numericText(countsDown: false))
                        .animation(.spring(response: 0.4, dampingFraction: 0.72), value: value)
                }
            }
        }
        .monospacedDigit()
    }
}

/// Sparkline scrolls the recent past, so a link dropping out or a transfer
/// ramping up is visible rather than only its current value.
struct Sparkline: View {
    let history: [Double]
    let colour: Color

    var body: some View {
        Canvas { context, size in
            guard history.count > 1 else { return }
            let peak = max(history.max() ?? 1, 1)
            let step = size.width / CGFloat(max(1, history.count - 1))

            var line = Path()
            var fill = Path()
            fill.move(to: CGPoint(x: 0, y: size.height))
            for (i, v) in history.enumerated() {
                let x = CGFloat(i) * step
                let y = size.height - CGFloat(v / peak) * (size.height - 3) - 1.5
                if i == 0 { line.move(to: CGPoint(x: x, y: y)) } else { line.addLine(to: CGPoint(x: x, y: y)) }
                fill.addLine(to: CGPoint(x: x, y: y))
            }
            fill.addLine(to: CGPoint(x: size.width, y: size.height))
            fill.closeSubpath()

            context.fill(fill, with: .linearGradient(
                Gradient(colors: [colour.opacity(0.35), colour.opacity(0.02)]),
                startPoint: .zero, endPoint: CGPoint(x: 0, y: size.height)
            ))
            context.stroke(line, with: .color(colour), lineWidth: 1.8)
        }
    }
}

/// LinkCard gives each uplink a face: its colour, whether it costs money, and
/// what it is contributing right now.
struct LinkCard: View {
    let link: LinkInfo
    let share: Double
    let bytes: Int64
    let colour: Color
    let idle: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 9) {
            HStack(spacing: 8) {
                Circle().fill(colour).frame(width: 9, height: 9)
                    .shadow(color: colour.opacity(idle ? 0 : 0.8), radius: idle ? 0 : 5)
                Text(link.label).font(.system(size: 14, weight: .medium))
                Spacer()
                if link.metered {
                    Text("billed")
                        .font(.system(size: 11, weight: .medium))
                        .padding(.horizontal, 6).padding(.vertical, 2)
                        .background(Theme.cost[0].opacity(0.18), in: Capsule())
                        .foregroundStyle(Theme.cost[0])
                }
            }
            Text(idle ? "—" : "\(Int(share * 100))%")
                .font(.system(size: 26, weight: .semibold)).monospacedDigit()
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Theme.pending)
                    Capsule().fill(colour)
                        .frame(width: max(3, geo.size.width * share))
                        .animation(.easeOut(duration: 0.4), value: share)
                }
            }
            .frame(height: 5)
            Text(idle ? "idle" : prettyBytes(bytes))
                .font(.system(size: 12)).foregroundStyle(Theme.inkSoft)
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Theme.surface, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .strokeBorder(idle ? Theme.rule : colour.opacity(0.35), lineWidth: 1)
        )
    }
}

/// Pulse makes a finished transfer announce itself once rather than silently
/// changing a label.
struct Pulse: ViewModifier {
    let trigger: Bool
    @State private var scale: CGFloat = 1

    func body(content: Content) -> some View {
        content
            .scaleEffect(scale)
            .onChange(of: trigger) { _, now in
                guard now else { return }
                withAnimation(.spring(response: 0.26, dampingFraction: 0.38)) { scale = 1.07 }
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) {
                    withAnimation(.spring(response: 0.5, dampingFraction: 0.7)) { scale = 1 }
                }
            }
    }
}

extension View {
    func pulse(on trigger: Bool) -> some View { modifier(Pulse(trigger: trigger)) }
}
